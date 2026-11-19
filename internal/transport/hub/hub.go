// Package hub adapts outer hub connections to end-to-end transport sessions.
package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

// ErrPeerBusy is shared with the outer protocol for errors.Is matching.
var ErrPeerBusy = router.ErrPeerBusy

// Wrap must authenticate the inner peer, honor ctx, and close conn with Session.
// A zero expect permits any authenticated peer (standalone wait connections).
type Wrap func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error)

// Options selects embedded acceptance or outbound registration. Embedded takes
// precedence over Endpoints; Listen uses the supplied Embedded when available.
type Options struct {
	Local       *identity.Local
	Wrap        Wrap
	Embedded    *router.Embedded
	Listen      string
	SetupLimits router.SetupLimits
	// Endpoints are standalone hubs on which Accept registers as a waiter.
	Endpoints []transport.Endpoint
	// Token is an optional standalone-hub credential, copied by New.
	Token []byte
}

// Connector uses its local identity for both pinned and direct pairing dials.
type Connector struct {
	o          Options
	ctx        context.Context
	cancel     context.CancelFunc
	serverDone chan struct{}
	gate       chan struct{}
	mu         sync.Mutex
	active     transport.Session
	closed     bool
}

// Register injects the inner security layer into the transport registry once,
// during application startup. Config accepts "listen"; relay endpoints use New.
func Register(wrap Wrap) {
	transport.Register("hub", func(cfg transport.Config, local *identity.Local) (transport.Connector, transport.Acceptor, error) {
		connector, acceptor, err := New(Options{Local: local, Wrap: wrap, Listen: cfg.Options["listen"]})
		if err != nil {
			return nil, nil, err
		}
		return connector, acceptor, nil
	})
}

// New owns listeners started by Listen. The returned acceptor must be closed;
// it is nil when neither embedded acceptance nor wait endpoints are configured.
func New(o Options) (*Connector, transport.Acceptor, error) {
	if o.Local == nil || o.Wrap == nil {
		return nil, nil, errors.New("transport/hub: Local and Wrap required")
	}
	o.Endpoints = append([]transport.Endpoint(nil), o.Endpoints...)
	o.Token = append([]byte(nil), o.Token...)
	ctx, cancel := context.WithCancel(context.Background())
	a := &Connector{o: o, ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1)}
	a.gate <- struct{}{}
	if o.Listen != "" {
		ln, err := net.Listen("tcp", o.Listen)
		if err != nil {
			cancel()
			return nil, nil, fmt.Errorf("transport/hub: listen: %w", err)
		}
		if a.o.Embedded == nil {
			a.o.Embedded = router.NewEmbedded()
		}
		a.serverDone = make(chan struct{})
		server := router.NewServer(embeddedServerOptions(o.Local, a.o.Embedded, o.SetupLimits))
		go func() {
			defer close(a.serverDone)
			_ = server.Serve(ctx, ln)
			cancel()
		}()
	}
	if a.o.Embedded == nil && len(o.Endpoints) == 0 {
		cancel()
		return a, nil, nil
	}
	return a, a, nil
}

func path(conn net.Conn, ep transport.Endpoint, peer identity.Fingerprint) transport.PathInfo {
	p := transport.PathInfo{Kind: transport.PathRelay, LocalAddr: conn.LocalAddr().String(), RemoteAddr: conn.RemoteAddr().String(), Via: ep.Addr}
	if ep.Hub == peer {
		p.Kind = transport.PathDirect
		p.Via = ""
	}
	return p
}

func (a *Connector) open(ctx context.Context, ep transport.Endpoint, role router.Role, target identity.Fingerprint) (transport.Session, error) {
	conn, err := router.Connect(ctx, ep.Addr, a.o.Local, ep.Hub, router.Request{Role: role, Target: target, Token: a.o.Token})
	if err != nil {
		return nil, err
	}
	expect := target
	if role == router.RoleWait {
		expect = identity.Fingerprint{}
	}
	return a.wrap(ctx, conn, role == router.RoleDial, expect, path(conn, ep, target), role == router.RoleWait)
}

// DialDirectPair binds the inner identity to the hub reached by this self-dial.
func (a *Connector) DialDirectPair(ctx context.Context, addr string) (transport.Session, error) {
	if a.isClosed() {
		return nil, net.ErrClosed
	}
	conn, hubID, err := router.ConnectBootstrap(ctx, addr, a.o.Local)
	if err != nil {
		return nil, err
	}
	p := transport.PathInfo{Kind: transport.PathDirect, LocalAddr: conn.LocalAddr().String(), RemoteAddr: conn.RemoteAddr().String()}
	return a.wrap(ctx, conn, true, hubID, p, false)
}

func (a *Connector) Dial(ctx context.Context, peer transport.PeerInfo) (transport.Session, error) {
	if a.isClosed() {
		return nil, net.ErrClosed
	}
	if peer.ID.IsZero() {
		return nil, errors.New("transport/hub: peer identity required")
	}
	return race(ctx, peer.Endpoints, func(ctx context.Context, ep transport.Endpoint) (transport.Session, error) {
		return a.open(ctx, ep, router.RoleDial, peer.ID)
	})
}

func (a *Connector) isClosed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closed
}

// race waits for every canceled attempt before returning. Only a fully wrapped
// session can win; a fast TCP connection alone cannot suppress working paths.
func race(ctx context.Context, endpoints []transport.Endpoint, attempt func(context.Context, transport.Endpoint) (transport.Session, error)) (transport.Session, error) {
	type result struct {
		session transport.Session
		err     error
		index   int
	}
	var candidates []transport.Endpoint
	for _, ep := range endpoints {
		if ep.Kind == "hub" {
			candidates = append(candidates, ep)
		}
	}
	if len(candidates) == 0 {
		return nil, errors.New("transport/hub: no hub endpoints")
	}
	results := make(chan result, len(candidates))
	cancels := make([]context.CancelFunc, len(candidates))
	for i, ep := range candidates {
		attemptCtx, cancel := context.WithCancel(ctx)
		cancels[i] = cancel
		go func() {
			s, err := attempt(attemptCtx, ep)
			results <- result{s, err, i}
		}()
	}
	var winner transport.Session
	var failures []error
	for range candidates {
		r := <-results
		if r.err == nil && winner == nil && ctx.Err() == nil {
			winner = ownSession(ctx, r.session, cancels[r.index])
			for i, cancel := range cancels {
				if i != r.index {
					cancel()
				}
			}
		} else {
			cancels[r.index]()
			if r.session != nil {
				_ = r.session.Close(context.Canceled)
			}
			if r.err != nil {
				failures = append(failures, r.err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		if winner != nil {
			_ = winner.Close(err)
		}
		return nil, err
	}
	if winner != nil {
		return winner, nil
	}
	return nil, fmt.Errorf("transport/hub: all endpoints failed: %w", errors.Join(failures...))
}

func (a *Connector) Accept(ctx context.Context) (transport.Session, error) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(a.ctx, cancel)
	// The winner's context remains valid until it is closed or the acceptor stops.
	success := false
	defer func() {
		if !success {
			stop()
			cancel()
		}
	}()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-a.gate:
	}
	defer func() { a.gate <- struct{}{} }()
	a.mu.Lock()
	active := a.active
	closed := a.closed
	a.mu.Unlock()
	if closed {
		return nil, net.ErrClosed
	}
	if active != nil {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-active.Done():
		}
	}
	var session transport.Session
	var err error
	if a.o.Embedded != nil {
		var conn net.Conn
		var peer identity.Fingerprint
		conn, peer, err = a.o.Embedded.Accept(ctx)
		if err == nil {
			p := transport.PathInfo{Kind: transport.PathDirect, LocalAddr: conn.LocalAddr().String(), RemoteAddr: conn.RemoteAddr().String()}
			session, err = a.wrap(ctx, conn, false, peer, p, false)
		}
	} else {
		session, err = race(ctx, a.o.Endpoints, func(ctx context.Context, ep transport.Endpoint) (transport.Session, error) {
			return a.open(ctx, ep, router.RoleWait, identity.Fingerprint{})
		})
	}
	if err != nil {
		return nil, err
	}
	session = ownSession(ctx, session, func() { stop(); cancel() })
	a.mu.Lock()
	if a.closed || ctx.Err() != nil {
		a.mu.Unlock()
		_ = session.Close(context.Canceled)
		return nil, context.Canceled
	}
	a.active = session
	a.mu.Unlock()
	success = true
	return session, nil
}

func (a *Connector) Close() error {
	a.mu.Lock()
	a.closed = true
	active := a.active
	a.mu.Unlock()
	a.cancel()
	if active != nil {
		_ = active.Close(net.ErrClosed)
	}
	if a.serverDone != nil {
		<-a.serverDone
	}
	return nil
}

// embeddedServerOptions admits only dials to local. Without this restriction a
// public Operator listener would route third-party wait/dial pairs, turning the
// Operator's bandwidth into an open relay.
func embeddedServerOptions(local *identity.Local, e *router.Embedded, limits router.SetupLimits) router.Options {
	self := local.Fingerprint()
	return router.Options{
		Local:       local,
		Embedded:    e,
		SetupLimits: limits,
		Authorize: func(_ identity.Fingerprint, role router.Role, target identity.Fingerprint) bool {
			return role == router.RoleDial && target == self
		},
	}
}
