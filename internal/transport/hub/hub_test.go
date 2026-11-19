package hub

import (
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

type relayPolicy struct{}

func (relayPolicy) Admit(identity.Fingerprint, router.Request, time.Time) (router.Grant, error) {
	return router.Grant{}, nil
}
func (relayPolicy) Valid(router.Grant, identity.Fingerprint) bool { return true }
func (relayPolicy) PairingOperator() identity.Fingerprint         { return identity.Fingerprint{} }

// stubWrap exchanges identities only for test assertions. It deliberately keeps
// ctx for the full session lifetime, like secure.WrapStream.
func stubWrap(self identity.Fingerprint) Wrap {
	return func(ctx context.Context, conn net.Conn, client bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
		var peer identity.Fingerprint
		if client {
			if _, err := conn.Write(self[:]); err != nil {
				return nil, err
			}
			if _, err := io.ReadFull(conn, peer[:]); err != nil {
				return nil, err
			}
		} else {
			if _, err := io.ReadFull(conn, peer[:]); err != nil {
				return nil, err
			}
			if _, err := conn.Write(self[:]); err != nil {
				return nil, err
			}
		}
		if !expect.IsZero() && peer != expect {
			return nil, errors.New("stub: wrong peer")
		}
		s := &stubSession{conn: conn, peer: peer, path: path, done: make(chan struct{}), ctx: ctx}
		context.AfterFunc(ctx, func() { _ = s.Close(ctx.Err()) })
		return s, nil
	}
}

type stubSession struct {
	conn net.Conn
	peer identity.Fingerprint
	path transport.PathInfo
	ctx  context.Context
	mu   sync.Mutex
	err  error
	once sync.Once
	done chan struct{}
}

func (s *stubSession) OpenStream(ctx context.Context) (transport.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &stubStream{s: s}, nil
}
func (s *stubSession) AcceptStream(ctx context.Context) (transport.Stream, error) {
	return s.OpenStream(ctx)
}
func (s *stubSession) Peer() identity.Fingerprint { return s.peer }
func (s *stubSession) Path() transport.PathInfo   { return s.path }
func (s *stubSession) Stats() transport.LinkStats { return transport.LinkStats{} }
func (s *stubSession) Done() <-chan struct{}      { return s.done }
func (s *stubSession) Err() error                 { s.mu.Lock(); defer s.mu.Unlock(); return s.err }
func (s *stubSession) Close(reason error) error {
	s.once.Do(func() {
		if reason == nil {
			reason = io.EOF
		}
		s.mu.Lock()
		s.err = reason
		s.mu.Unlock()
		_ = s.conn.Close()
		close(s.done)
	})
	return nil
}

type stubStream struct{ s *stubSession }

func (s *stubStream) Read(p []byte) (int, error)  { return s.s.conn.Read(p) }
func (s *stubStream) Write(p []byte) (int, error) { return s.s.conn.Write(p) }
func (s *stubStream) Close() error                { return s.s.Close(nil) }
func (s *stubStream) CloseWrite() error           { return transport.ErrUnsupported }

func id(t *testing.T) *identity.Local {
	t.Helper()
	l, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func startHub(t *testing.T, h *identity.Local, e *router.Embedded) transport.Endpoint {
	t.Helper()
	options := router.Options{Local: h, Embedded: e}
	if e == nil {
		options.Policy = relayPolicy{}
	}
	return startTestHub(t, options)
}

func startTestHub(t *testing.T, options router.Options) transport.Endpoint {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- router.NewServer(options).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server handler leaked")
		}
	})
	return transport.Endpoint{Kind: "hub", Addr: ln.Addr().String(), Hub: options.Local.Fingerprint()}
}

func build(t *testing.T, o Options) (transport.Connector, transport.Acceptor) {
	t.Helper()
	c, a, err := New(o)
	if err != nil {
		t.Fatal(err)
	}
	if a != nil {
		t.Cleanup(func() { _ = a.Close() })
	}
	return c, a
}

func closeSession(t *testing.T, s transport.Session) {
	t.Helper()
	t.Cleanup(func() { _ = s.Close(nil) })
}

type acceptedResult struct {
	session transport.Session
	err     error
}

func asyncAccept(t *testing.T, ctx context.Context, a transport.Acceptor) <-chan acceptedResult {
	t.Helper()
	result := make(chan acceptedResult, 1)
	go func() { s, err := a.Accept(ctx); result <- acceptedResult{s, err} }()
	return result
}

func receive(t *testing.T, ch <-chan acceptedResult) transport.Session {
	t.Helper()
	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		closeSession(t, r.session)
		return r.session
	case <-time.After(3 * time.Second):
		t.Fatal("Accept did not finish")
		return nil
	}
}

func transfer(t *testing.T, a, b transport.Session) {
	t.Helper()
	out, err := a.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	in, err := b.AcceptStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := out.Write([]byte("opaque bytes")); done <- err }()
	got := make([]byte, len("opaque bytes"))
	if _, err := io.ReadFull(in, got); err != nil || string(got) != "opaque bytes" {
		t.Fatalf("transfer: %q %v", got, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestEmbeddedSessionsAndReconnect(t *testing.T) {
	b, a := id(t), id(t)
	e := router.NewEmbedded()
	ep := startHub(t, b, e)
	dialer, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	_, acceptor := build(t, Options{Local: b, Wrap: stubWrap(b.Fingerprint()), Embedded: e})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	for range 2 {
		pending := asyncAccept(t, ctx, acceptor)
		s, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}})
		if err != nil {
			t.Fatal(err)
		}
		closeSession(t, s)
		r := receive(t, pending)
		if s.Peer() != b.Fingerprint() || r.Peer() != a.Fingerprint() || s.Path().Kind != transport.PathDirect || r.Path().Kind != transport.PathDirect || s.Path().Via != "" {
			t.Fatal("wrong direct path or peer")
		}
		// Successful setup must not cancel the context retained by Wrap.
		select {
		case <-s.Done():
			t.Fatal("setup canceled winner")
		default:
		}
		transfer(t, s, r)
		if _, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}}); !errors.Is(err, ErrPeerBusy) {
			t.Fatalf("busy: %v", err)
		}
		_ = r.Close(nil)
		_ = s.Close(nil)
	}
}

func TestRelaySessions(t *testing.T) {
	h, a, b := id(t), id(t), id(t)
	ep := startHub(t, h, nil)
	waiting := make(chan struct{})
	wrap := stubWrap(b.Fingerprint())
	_, acceptor := build(t, Options{Local: b, Endpoints: []transport.Endpoint{ep}, Wrap: func(ctx context.Context, c net.Conn, client bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
		close(waiting)
		return wrap(ctx, c, client, expect, p)
	}})
	dialer, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pending := asyncAccept(t, ctx, acceptor)
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	s, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}})
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	r := receive(t, pending)
	if s.Path().Kind != transport.PathRelay || r.Path().Kind != transport.PathRelay || s.Path().Via != ep.Addr || r.Path().Via != ep.Addr || r.Peer() != a.Fingerprint() {
		t.Fatal("relay metadata")
	}
	transfer(t, s, r)
	transfer(t, r, s)
}

func TestCandidateRace(t *testing.T) {
	a, b := id(t), id(t)
	slowE, fastE := router.NewEmbedded(), router.NewEmbedded()
	slow, fast := startHub(t, b, slowE), startHub(t, b, fastE)
	slowStarted := make(chan struct{})
	_, fastAcceptor := build(t, Options{Local: b, Embedded: fastE, Wrap: func(ctx context.Context, conn net.Conn, client bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
		select {
		case <-slowStarted:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return stubWrap(b.Fingerprint())(ctx, conn, client, expect, p)
	}})
	_, slowAcceptor := build(t, Options{Local: b, Embedded: slowE, Wrap: func(ctx context.Context, conn net.Conn, _ bool, _ identity.Fingerprint, _ transport.PathInfo) (transport.Session, error) {
		_, err := conn.Read(make([]byte, 4096))
		close(slowStarted)
		if err == nil {
			_, err = conn.Read(make([]byte, 1))
		}
		return nil, err
	}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	slowResult := asyncAccept(t, ctx, slowAcceptor)
	fastResult := asyncAccept(t, ctx, fastAcceptor)
	dialer, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	s, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{slow, {Kind: "other"}, fast}})
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	r := receive(t, fastResult)
	select {
	case r := <-slowResult:
		if r.err == nil {
			_ = r.session.Close(nil)
			t.Fatal("unfinished candidate won")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("canceled candidate kept a connection")
	}
	transfer(t, s, r)
}

func TestCancelDialAndAccept(t *testing.T) {
	a, b := id(t), id(t)
	e := router.NewEmbedded()
	ep := startHub(t, b, e)
	dialer, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cancel dial: %v", err)
	}
	_, acc := build(t, Options{Local: b, Embedded: router.NewEmbedded(), Wrap: stubWrap(b.Fingerprint())})
	ctx, stop := context.WithCancel(t.Context())
	pending := asyncAccept(t, ctx, acc)
	stop()
	select {
	case r := <-pending:
		if !errors.Is(r.err, context.Canceled) {
			t.Fatal(r.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Accept leaked")
	}
	pending = asyncAccept(t, t.Context(), acc)
	_ = acc.Close()
	select {
	case r := <-pending:
		if r.err == nil {
			t.Fatal("closed Accept succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not interrupt Accept")
	}
}

func TestCloseDuringWrapReleasesEmbedded(t *testing.T) {
	a, b := id(t), id(t)
	e := router.NewEmbedded()
	ep := startHub(t, b, e)
	started := make(chan struct{})
	_, acc := build(t, Options{Local: b, Embedded: e, Wrap: func(ctx context.Context, c net.Conn, _ bool, _ identity.Fingerprint, _ transport.PathInfo) (transport.Session, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	}})
	pending := asyncAccept(t, t.Context(), acc)
	c, err := router.Connect(t.Context(), ep.Addr, a, ep.Hub, router.Request{Role: router.RoleDial, Target: b.Fingerprint()})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Wrap did not start")
	}
	_ = acc.Close()
	select {
	case r := <-pending:
		if r.err == nil {
			t.Fatal("canceled Wrap accepted")
		}
	case <-time.After(time.Second):
		t.Fatal("Wrap did not stop")
	}
	// A new acceptor on the same Embedded can accept after setup failure.
	_, next := build(t, Options{Local: b, Embedded: e, Wrap: stubWrap(b.Fingerprint())})
	result := asyncAccept(t, t.Context(), next)
	d, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	s, err := d.Dial(t.Context(), transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}})
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	receive(t, result)
}

func TestInnerSetupTimerAndLifetime(t *testing.T) {
	for _, succeed := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "success"}[succeed], func(t *testing.T) {
			a, b := net.Pipe()
			defer a.Close()
			defer b.Close()
			var retained context.Context
			adapter := &Connector{o: Options{Wrap: func(ctx context.Context, conn net.Conn, _ bool, _ identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
				retained = ctx
				if _, ok := ctx.Deadline(); ok {
					t.Error("setup deadline leaked into session context")
				}
				c := conn.(*setupConn)
				c.mu.Lock()
				c.timer.Reset(30 * time.Millisecond)
				c.mu.Unlock()
				if !succeed {
					<-ctx.Done()
					return nil, context.Cause(ctx)
				}
				s := &stubSession{conn: conn, ctx: ctx, path: p, done: make(chan struct{})}
				context.AfterFunc(ctx, func() { _ = s.Close(ctx.Err()) })
				return s, nil
			}}}
			s, err := adapter.wrap(t.Context(), a, true, identity.Fingerprint{1}, transport.PathInfo{}, nil)
			if !succeed {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("setup timeout: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			closeSession(t, s)
			select {
			case <-retained.Done():
				t.Fatal("successful setup canceled session")
			case <-time.After(60 * time.Millisecond):
			}
			_ = s.Close(nil)
			if retained.Err() == nil {
				t.Fatal("session Close did not release context")
			}
		})
	}
}

func TestIdleWaitHasNoSetupDeadline(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	started := make(chan struct{})
	adapter := &Connector{o: Options{Wrap: func(ctx context.Context, conn net.Conn, _ bool, _ identity.Fingerprint, _ transport.PathInfo) (transport.Session, error) {
		c := conn.(*setupConn)
		c.mu.Lock()
		if c.started || c.timer != nil {
			t.Error("idle wait has a setup timer")
		}
		c.mu.Unlock()
		close(started)
		_, err := conn.Read(make([]byte, 1))
		return nil, err
	}}}
	done := make(chan error, 1)
	go func() {
		_, err := adapter.wrap(ctx, a, false, identity.Fingerprint{}, transport.PathInfo{}, func() {})
		done <- err
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wait cancellation stuck")
	}
}

var registrationOnce sync.Once

func TestRegistryAndValidation(t *testing.T) {
	l := id(t)
	registrationOnce.Do(func() { Register(stubWrap(l.Fingerprint())) })
	c, a, err := transport.New(transport.Config{Kind: "hub"}, l)
	if err != nil || c == nil || a != nil {
		t.Fatalf("factory: %v %v %v", c, a, err)
	}
	if _, _, err := New(Options{Local: l}); err == nil {
		t.Fatal("nil Wrap accepted")
	}
	if _, _, err := New(Options{Wrap: stubWrap(l.Fingerprint())}); err == nil {
		t.Fatal("nil identity accepted")
	}
	if _, err := c.Dial(t.Context(), transport.PeerInfo{ID: l.Fingerprint(), Endpoints: []transport.Endpoint{{Kind: "other"}}}); err == nil {
		t.Fatal("empty candidates accepted")
	}
	if _, err := c.Dial(t.Context(), transport.PeerInfo{}); err == nil {
		t.Fatal("zero peer accepted")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if _, _, err := New(Options{Local: l, Wrap: stubWrap(l.Fingerprint()), Listen: ln.Addr().String()}); err == nil {
		t.Fatal("occupied listener accepted")
	}
	_, own := build(t, Options{Local: l, Wrap: stubWrap(l.Fingerprint()), Listen: "127.0.0.1:0"})
	if err := own.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRaceDisposesSimultaneousSuccess(t *testing.T) {
	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	var mu sync.Mutex
	var sessions []*stubSession
	var endpoints []transport.Endpoint
	for range n {
		endpoints = append(endpoints, transport.Endpoint{Kind: "hub"})
	}
	s, err := race(t.Context(), endpoints, func(ctx context.Context, _ transport.Endpoint) (transport.Session, error) {
		c, peer := net.Pipe()
		_ = peer.Close()
		s := &stubSession{conn: c, ctx: ctx, done: make(chan struct{})}
		mu.Lock()
		sessions = append(sessions, s)
		mu.Unlock()
		wg.Done()
		wg.Wait()
		return s, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close(nil)
	for _, candidate := range sessions {
		select {
		case <-candidate.Done():
		default:
			t.Fatal("successful loser leaked")
		}
		if candidate.ctx.Err() == nil {
			t.Fatal("candidate context retained")
		}
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 3 {
		t.Run("cycle", func(t *testing.T) { TestEmbeddedSessionsAndReconnect(t) })
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines before=%d after=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestEmbeddedListenerIsNotARelay(t *testing.T) {
	h, a, b := id(t), id(t), id(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- router.NewServer(embeddedServerOptions(h, router.NewEmbedded(), router.SetupLimits{})).Serve(ctx, ln)
	}()
	defer func() {
		cancel()
		<-done
	}()
	addr := ln.Addr().String()
	for name, c := range map[string]struct {
		local *identity.Local
		req   router.Request
	}{
		"wait":             {b, router.Request{Role: router.RoleWait}},
		"targeted wait":    {b, router.Request{Role: router.RoleWait, Target: a.Fingerprint()}},
		"third-party dial": {a, router.Request{Role: router.RoleDial, Target: b.Fingerprint()}},
	} {
		conn, err := router.Connect(t.Context(), addr, c.local, h.Fingerprint(), c.req)
		if conn != nil {
			_ = conn.Close()
		}
		if !errors.Is(err, router.ErrUnauthorized) {
			t.Errorf("%s: err = %v, want unauthorized", name, err)
		}
	}
	conn, err := router.Connect(t.Context(), addr, a, h.Fingerprint(), router.Request{Role: router.RoleDial, Target: h.Fingerprint()})
	if err != nil {
		t.Fatalf("self dial: %v", err)
	}
	_ = conn.Close()
}

func TestAcceptAfterSilentPair(t *testing.T) {
	h, a, b := id(t), id(t), id(t)
	ep := startTestHub(t, router.Options{Local: h, Policy: relayPolicy{}, SetupLimits: router.SetupLimits{Timeout: 150 * time.Millisecond}})
	registered := make(chan error, 2)
	_, acceptor := build(t, Options{Local: b, Endpoints: []transport.Endpoint{ep}, Wrap: stubWrap(b.Fingerprint()), OnRegister: func(_ identity.Fingerprint, _ []byte, err error) { registered <- err }})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	register := func() <-chan acceptedResult {
		pending := asyncAccept(t, ctx, acceptor)
		select {
		case err := <-registered:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		return pending
	}
	pending := register()
	conn, err := router.Connect(ctx, ep.Addr, a, ep.Hub, router.Request{Role: router.RoleDial, Target: b.Fingerprint()})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	select {
	case result := <-pending:
		if result.err == nil || ctx.Err() != nil {
			t.Fatalf("silent pair: %v", result.err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	pending = register()
	dialer, _ := build(t, Options{Local: a, Wrap: stubWrap(a.Fingerprint())})
	s, err := dialer.Dial(ctx, transport.PeerInfo{ID: b.Fingerprint(), Endpoints: []transport.Endpoint{ep}})
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	transfer(t, s, receive(t, pending))
}
