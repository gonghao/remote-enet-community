package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunnel"
)

const fixtureBody = "remote-enet local TUN fixture\n"

type flow struct {
	Kind               string `json:"kind"`
	Target             string `json:"target"`
	Origin             string `json:"origin"`
	OriginMatchesLocal bool   `json:"origin_matches_local"`
	Error              string `json:"error,omitempty"`
}

type fixture struct {
	operator      *session.Session
	client        *session.Session
	transports    [2]transport.Session
	cancel        context.CancelFunc
	http          *http.Server
	httpDone      chan error
	httpAddr      string
	sshAddr       string
	target, local netip.Addr
	mu            sync.Mutex
	closing       bool
	flows         []flow
	workers       sync.WaitGroup
}

func startFixture(ctx context.Context, local, target netip.Addr, sshAddr string) (*fixture, error) {
	endpoint, err := netip.ParseAddrPort(sshAddr)
	if err != nil || endpoint.Addr() != netip.MustParseAddr("127.0.0.1") || endpoint.Port() == 0 {
		return nil, fmt.Errorf("SSH fixture endpoint must be 127.0.0.1 with a nonzero port")
	}
	operatorID, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	clientID, err := identity.Generate()
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listen HTTP fixture: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	f := &fixture{cancel: cancel, sshAddr: sshAddr, httpAddr: listener.Addr().String(), target: target, local: local, httpDone: make(chan error, 1), flows: []flow{}}
	f.http = &http.Server{ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 3 * time.Second, WriteTimeout: 3 * time.Second, IdleTimeout: time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		if f.closing {
			f.mu.Unlock()
			return
		}
		f.workers.Add(1)
		f.mu.Unlock()
		defer f.workers.Done()
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Connection", "close")
		_, _ = io.WriteString(w, fixtureBody)
	})}
	go func() { f.httpDone <- f.http.Serve(listener) }()
	fail := func(err error) (*fixture, error) { return nil, errors.Join(err, f.Close()) }
	a, b := net.Pipe()
	handshakeDeadline := time.Now().Add(5 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(handshakeDeadline) {
		handshakeDeadline = end
	}
	_ = a.SetDeadline(handshakeDeadline)
	_ = b.SetDeadline(handshakeDeadline)
	type wrapped struct {
		session transport.Session
		err     error
	}
	wrappedClient := make(chan wrapped, 1)
	go func() {
		s, err := secure.WrapStream(ctx, b, secure.Opts{Local: clientID, Expect: operatorID.Fingerprint(), IsClient: false})
		wrappedClient <- wrapped{s, err}
	}()
	ts, wrapErr := secure.WrapStream(ctx, a, secure.Opts{Local: operatorID, Expect: clientID.Fingerprint(), IsClient: true})
	if wrapErr != nil {
		cancel()
		_ = a.Close()
		_ = b.Close()
	}
	other := <-wrappedClient
	_ = a.SetDeadline(time.Time{})
	_ = b.SetDeadline(time.Time{})
	f.transports = [2]transport.Session{ts, other.session}
	if wrapErr != nil || other.err != nil {
		return fail(fmt.Errorf("fixture mTLS: %w", errors.Join(wrapErr, other.err)))
	}
	type running struct {
		session *session.Session
		err     error
	}
	startedClient := make(chan running, 1)
	go func() {
		s, err := session.Run(ctx, other.session, session.Options{Role: session.RoleClient, Capabilities: []string{"tcp"}, Trusted: func(peer identity.Fingerprint) bool { return peer == operatorID.Fingerprint() }, OnTunnel: f.handle})
		startedClient <- running{s, err}
	}()
	operator, runErr := session.Run(ctx, ts, session.Options{Role: session.RoleOperator, Initiator: true, Capabilities: []string{"tcp"}, Trusted: func(peer identity.Fingerprint) bool { return peer == clientID.Fingerprint() }})
	if runErr != nil {
		cancel()
	}
	client := <-startedClient
	f.operator, f.client = operator, client.session
	if runErr != nil || client.err != nil {
		return fail(fmt.Errorf("fixture session: %w", errors.Join(runErr, client.err)))
	}
	return f, nil
}

func (f *fixture) destination(h proto.OpenTunnel) (string, error) {
	if h.Kind != "tcp" {
		return "", fmt.Errorf("fixture rejects tunnel kind %q", h.Kind)
	}
	src, err := netip.ParseAddrPort(h.Origin)
	if err != nil || src.Addr() != f.local || src.Port() == 0 {
		return "", fmt.Errorf("fixture origin must use configured local IPv4")
	}
	switch h.Target {
	case netip.AddrPortFrom(f.target, 22).String():
		return f.sshAddr, nil
	case netip.AddrPortFrom(f.target, 8080).String():
		return f.httpAddr, nil
	default:
		return "", fmt.Errorf("fixture rejects target %q", h.Target)
	}
}

func (f *fixture) handle(ctx context.Context, h proto.OpenTunnel, s transport.Stream) {
	defer s.Close()
	f.mu.Lock()
	if f.closing {
		f.mu.Unlock()
		return
	}
	f.workers.Add(1)
	f.mu.Unlock()
	defer f.workers.Done()
	dst, err := f.destination(h)
	src, parseErr := netip.ParseAddrPort(h.Origin)
	result := flow{Kind: h.Kind, Target: h.Target, Origin: h.Origin, OriginMatchesLocal: parseErr == nil && src.Addr() == f.local && src.Port() != 0}
	var pipeErr error
	defer func() { f.recordFlow(result, pipeErr) }()
	if err != nil {
		result.Error = err.Error()
		return
	}

	dialer := net.Dialer{Timeout: 3 * time.Second}
	conn, dialErr := dialer.DialContext(ctx, "tcp4", dst)
	if dialErr != nil {
		result.Error = fmt.Errorf("fixture loopback dial: %w", dialErr).Error()
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = conn.SetDeadline(deadline)
	pipeErr = tunnel.Pipe(ctx, s, conn, nil)
}

func (f *fixture) recordFlow(result flow, pipeErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Destination and dial errors are final, even when cleanup races reporting.
	if result.Error == "" && pipeErr != nil && !f.closing {
		result.Error = pipeErr.Error()
	}
	f.flows = append(f.flows, result)
}

func (f *fixture) beginClose() {
	f.mu.Lock()
	f.closing = true
	f.mu.Unlock()
}

func (f *fixture) Close() error {
	f.beginClose()
	f.cancel()
	var errs []error
	for _, ts := range f.transports {
		if ts != nil {
			errs = append(errs, ts.Close(context.Canceled))
			<-ts.Done()
		}
	}
	for _, s := range []*session.Session{f.operator, f.client} {
		if s != nil {
			<-s.Done()
		}
	}
	shutdownCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	shutdownErr := f.http.Shutdown(shutdownCtx)
	stop()
	errs = append(errs, shutdownErr)
	if shutdownErr != nil {
		errs = append(errs, f.http.Close())
	}
	f.workers.Wait()
	if err := <-f.httpDone; !errors.Is(err, http.ErrServerClosed) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (f *fixture) snapshot() []flow {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]flow{}, f.flows...)
}
