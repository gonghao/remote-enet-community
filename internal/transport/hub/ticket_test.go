package hub

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/secure"
	"remote-enet/internal/transport"
)

func TestTicketSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	operator, client := id(t), id(t)
	op, cl := operator.Fingerprint(), client.Fingerprint()
	waitToken, dialToken := []byte{0, 0xff, 'w'}, []byte{0xff, 0, 'd'}
	otherWait, otherDial := []byte("busy wait"), []byte("busy dial")
	var waits, dials atomic.Int32
	ep := startTestHub(t, router.Options{Local: id(t), Policy: testPolicy{admit: func(dev identity.Fingerprint, req router.Request, _ time.Time) (router.Grant, error) {
		switch {
		case req.Role == router.RoleWait && dev == op && req.Target == cl && (bytes.Equal(req.Token, waitToken) || bytes.Equal(req.Token, otherWait)):
			waits.Add(1)
		case req.Role == router.RoleDial && dev == cl && req.Target == op && (bytes.Equal(req.Token, dialToken) || bytes.Equal(req.Token, otherDial)):
			dials.Add(1)
		default:
			t.Error("incorrect role, device, target or ticket")
			return router.Grant{}, router.ErrUnauthorized
		}
		return router.Grant{}, nil
	}}})
	ep.Ticket = "legacy token"
	registered := make(chan struct{})
	waiter, acc, err := New(Options{
		Local: operator, Endpoints: []transport.Endpoint{ep},
		WaitToken: func(context.Context, identity.Fingerprint) ([]byte, error) {
			t.Error("WaitTicket called WaitToken")
			return nil, router.ErrUnauthorized
		},
		OnRegister: func(identity.Fingerprint, []byte, error) { t.Error("WaitTicket called OnRegister") },
		Wrap: func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
			if isClient || expect != cl {
				t.Error("incorrect wait Wrap role or peer pin")
			}
			checkSetupTimer(t, conn, false)
			close(registered)
			first := &firstReadConn{Conn: conn, pause: func() { checkSetupTimer(t, conn, true) }}
			return secure.WrapStream(ctx, first, secure.Opts{Local: operator, IsClient: isClient, Expect: expect, Path: p})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acc.Close() })
	dialer := ticketConnector(t, client, func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
		if !isClient || expect != op {
			t.Error("incorrect dial Wrap role or peer pin")
		}
		checkSetupTimer(t, conn, true)
		return secure.WrapStream(ctx, conn, secure.Opts{Local: client, IsClient: isClient, Expect: expect, Path: p})
	})

	// Before the waiter registers, the caller gets the rejection immediately.
	s, err := dialer.DialTicket(ctx, ep, op, dialToken)
	requireTicketError(t, ErrUnknownPeer, s, err)
	pending := startTicketWait(ctx, waiter, ep, cl, waitToken)
	waitSignal(t, ctx, registered)
	waiter.Refresh()
	duplicate, err := waiter.WaitTicket(ctx, ep, cl, otherWait)
	requireTicketError(t, ErrPeerBusy, duplicate, err)
	s, err = dialer.DialTicket(ctx, ep, op, dialToken)
	if err != nil {
		t.Fatal(err)
	}
	closeSession(t, s)
	r := receive(t, pending)
	for _, session := range []transport.Session{s, r} {
		p := session.Path()
		if p.Hub != ep.Hub || p.Kind != transport.PathRelay || p.Via != ep.Addr {
			t.Fatalf("wrong relay path: %+v", p)
		}
	}
	if s.Peer() != op || r.Peer() != cl {
		t.Fatal("wrong verified inner peer")
	}
	duplicate, err = dialer.DialTicket(ctx, ep, op, otherDial)
	requireTicketError(t, ErrPeerBusy, duplicate, err)
	if waits.Load() != 2 || dials.Load() != 3 {
		t.Fatalf("admission counts: wait=%d dial=%d", waits.Load(), dials.Load())
	}
	transfer(t, s, r)
	transfer(t, r, s)
	cancel()
	drained, stop := context.WithTimeout(t.Context(), time.Second)
	defer stop()
	waitSignal(t, drained, s.Done())
	waitSignal(t, drained, r.Done())
}

func TestTicketRejections(t *testing.T) {
	noWrap := func(context.Context, net.Conn, bool, identity.Fingerprint, transport.PathInfo) (transport.Session, error) {
		t.Error("outer rejection reached Wrap")
		return nil, errors.New("unexpected Wrap")
	}
	local, peer := id(t), id(t)
	c := ticketConnector(t, local, noWrap)
	var admitted atomic.Int32
	ep := startTestHub(t, router.Options{Local: id(t), Policy: testPolicy{admit: func(identity.Fingerprint, router.Request, time.Time) (router.Grant, error) {
		admitted.Add(1)
		return router.Grant{}, router.ErrUnauthorized
	}}})
	for _, tc := range []struct {
		name string
		call func(context.Context, transport.Endpoint, identity.Fingerprint, []byte) (transport.Session, error)
	}{
		{"wait", c.WaitTicket},
		{"dial", c.DialTicket},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			token := []byte("denied")
			s, err := tc.call(ctx, ep, peer.Fingerprint(), token)
			requireTicketError(t, ErrUnauthorized, s, err)

			bad := ep
			bad.Hub = local.Fingerprint()
			s, err = tc.call(ctx, bad, peer.Fingerprint(), token)
			requireTicketError(t, ErrHubFingerprintMismatch, s, err)
			if !errors.Is(err, identity.ErrFingerprintMismatch) {
				t.Fatalf("lost underlying fingerprint mismatch: %v", err)
			}

			bad = ep
			bad.Addr = "127.0.0.1:0"
			s, err = tc.call(ctx, bad, peer.Fingerprint(), token)
			var opErr *net.OpError
			if s != nil || !errors.As(err, &opErr) || opErr.Op != "dial" {
				t.Fatalf("expected network dial error, got session=%v err=%v", s, err)
			}
		})
	}
	if admitted.Load() != 2 {
		t.Fatalf("admission count = %d, want 2", admitted.Load())
	}
}

func TestTicketInnerPinMismatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	ep := startTestHub(t, router.Options{Local: id(t), Policy: relayPolicy{}})
	operator, client, wrongInner := id(t), id(t), id(t)
	registered := make(chan struct{})
	waiter := ticketConnector(t, operator, func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
		close(registered)
		return secure.WrapStream(ctx, conn, secure.Opts{Local: wrongInner, IsClient: isClient, Expect: expect, Path: p})
	})
	dialer := ticketConnector(t, client, func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
		return secure.WrapStream(ctx, conn, secure.Opts{Local: client, IsClient: isClient, Expect: expect, Path: p})
	})
	pending := startTicketWait(ctx, waiter, ep, client.Fingerprint(), []byte("wait"))
	waitSignal(t, ctx, registered)
	s, err := dialer.DialTicket(ctx, ep, operator.Fingerprint(), []byte("dial"))
	requireTicketError(t, identity.ErrFingerprintMismatch, s, err)
	if errors.Is(err, ErrHubFingerprintMismatch) {
		t.Fatalf("inner pin error classified as an outer mismatch: %v", err)
	}
	select {
	case r := <-pending:
		if r.session != nil {
			_ = r.session.Close(nil)
			t.Fatal("wait succeeded after dial rejected its inner certificate")
		}
	case <-ctx.Done():
		t.Fatal("wait did not exit after inner pin failure")
	}
}

func TestDialTicketCancelWhileReadingStatus(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	received := make(chan struct{})
	release := make(chan struct{})
	ep := startTestHub(t, router.Options{Local: id(t), Policy: testPolicy{admit: func(identity.Fingerprint, router.Request, time.Time) (router.Grant, error) {
		close(received)
		<-release
		return router.Grant{}, router.ErrUnauthorized
	}}})
	defer close(release)
	local, peer := id(t), id(t)
	dialer := ticketConnector(t, local, stubWrap(local.Fingerprint()))
	dialCtx, stop := context.WithCancel(ctx)
	defer stop()
	result := make(chan acceptedResult, 1)
	go func() {
		s, err := dialer.DialTicket(dialCtx, ep, peer.Fingerprint(), []byte("dial"))
		result <- acceptedResult{s, err}
	}()
	waitSignal(t, ctx, received)
	stop()
	select {
	case r := <-result:
		requireTicketError(t, context.Canceled, r.session, r.err)
	case <-ctx.Done():
		t.Fatal("canceled dial blocked on outer status")
	}
}

func TestTicketCancellationReleasesIdleWait(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 3 {
		t.Run("cycle", func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			ep := startTestHub(t, router.Options{Local: id(t), Policy: relayPolicy{}})
			operator, client := id(t), id(t)
			registered := make(chan struct{})
			waiter := ticketConnector(t, operator, func(_ context.Context, conn net.Conn, _ bool, _ identity.Fingerprint, _ transport.PathInfo) (transport.Session, error) {
				checkSetupTimer(t, conn, false)
				close(registered)
				_, err := io.ReadFull(conn, make([]byte, 1))
				return nil, err
			})
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			waiting := startTicketWait(waitCtx, waiter, ep, client.Fingerprint(), []byte("wait"))
			waitSignal(t, ctx, registered)
			stop()
			select {
			case r := <-waiting:
				requireTicketError(t, context.Canceled, r.session, r.err)
			case <-ctx.Done():
				t.Fatal("canceled wait blocked on first read")
			}
		})
	}
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines before=%d after=%d", before, runtime.NumGoroutine())
		}
		time.Sleep(time.Millisecond)
	}
}

func ticketConnector(t *testing.T, local *identity.Local, wrap Wrap) *Connector {
	t.Helper()
	c, _, err := New(Options{Local: local, Wrap: wrap})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func startTicketWait(ctx context.Context, c *Connector, ep transport.Endpoint, peer identity.Fingerprint, token []byte) <-chan acceptedResult {
	result := make(chan acceptedResult, 1)
	go func() {
		s, err := c.WaitTicket(ctx, ep, peer, token)
		result <- acceptedResult{s, err}
	}()
	return result
}

func checkSetupTimer(t *testing.T, conn net.Conn, want bool) {
	t.Helper()
	c := conn.(*setupConn)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started != want || (c.timer != nil) != want {
		t.Errorf("inner setup timer armed = %t, want %t", c.started, want)
	}
}

func requireTicketError(t *testing.T, want error, s transport.Session, err error) {
	t.Helper()
	if s != nil {
		_ = s.Close(nil)
	}
	if s != nil || !errors.Is(err, want) {
		t.Fatalf("session=%v err=%v, want %v", s, err, want)
	}
}
