package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/secure"
	"remote-enet/internal/transport"
)

type testPolicy struct {
	relayPolicy
	operator identity.Fingerprint
	admit    func(identity.Fingerprint, router.Request, time.Time) (router.Grant, error)
}

func (p testPolicy) PairingOperator() identity.Fingerprint { return p.operator }
func (p testPolicy) Admit(dev identity.Fingerprint, req router.Request, now time.Time) (router.Grant, error) {
	return p.admit(dev, req, now)
}

func TestEndpointTickets(t *testing.T) {
	local, target := id(t), id(t)
	dialer, _ := build(t, Options{Local: local, Wrap: stubWrap(local.Fingerprint())})
	var observed atomic.Int32
	var endpoints []transport.Endpoint
	for _, ticket := range []string{"\x00\xffA", "\x00\xffB"} {
		p := testPolicy{admit: func(dev identity.Fingerprint, req router.Request, _ time.Time) (router.Grant, error) {
			if dev != local.Fingerprint() || req.Role != router.RoleDial || req.Target != target.Fingerprint() || string(req.Token) != ticket {
				t.Errorf("incorrect dial on endpoint %q: %+v", ticket, req)
			}
			observed.Add(1)
			return router.Grant{}, router.ErrUnauthorized
		}}
		ep, _ := startTestHub(t, router.Options{Local: id(t), Policy: p})
		ep.Ticket = ticket
		endpoints = append(endpoints, ep)
	}
	_, err := dialer.Dial(t.Context(), transport.PeerInfo{ID: target.Fingerprint(), Endpoints: endpoints})
	if int(observed.Load()) != len(endpoints) {
		t.Fatal("dial did not try both endpoints")
	}
	if !errors.Is(err, router.ErrUnauthorized) {
		t.Fatal(err)
	}
}

// firstReadConn holds the handshake after transport has delivered its first byte.
type firstReadConn struct {
	net.Conn
	once  sync.Once
	pause func()
}

func (c *firstReadConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.once.Do(c.pause)
	}
	return n, err
}

func TestBootstrapAndRefresh(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "embedded", false: "standalone"}[direct], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			op, client := id(t), id(t)
			host := op
			registered := make(chan struct{}, 1)
			firstRead, resume := make(chan struct{}), make(chan struct{})
			var seq atomic.Int32
			opOpts := Options{Local: op, Wrap: func(ctx context.Context, conn net.Conn, client bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
				if !direct {
					select {
					case registered <- struct{}{}:
					case <-ctx.Done():
						return nil, ctx.Err()
					}
					conn = &firstReadConn{Conn: conn, pause: func() {
						close(firstRead)
						select {
						case <-resume:
						case <-ctx.Done():
						}
					}}
				}
				return secure.WrapStream(ctx, conn, secure.Opts{Local: op, IsClient: client, Expect: expect, Path: p})
			}}
			var ep transport.Endpoint
			if direct {
				opOpts.Embedded = router.NewEmbedded()
				ep = startHub(t, host, opOpts.Embedded)
			} else {
				host = id(t)
				policy := testPolicy{operator: op.Fingerprint(), admit: func(dev identity.Fingerprint, req router.Request, now time.Time) (router.Grant, error) {
					if req.Role == router.RoleWait {
						if dev != op.Fingerprint() || !req.Target.IsZero() || string(req.Token) != fmt.Sprint(seq.Load()) {
							t.Error("incorrect wait registration")
						}
						return router.Grant{PairingUntil: now.Add(time.Minute)}, nil
					}
					if req.Target != host.Fingerprint() || len(req.Token) != 0 {
						t.Error("incorrect bootstrap request")
					}
					return router.Grant{}, nil
				}}
				ep, _ = startTestHub(t, router.Options{Local: host, Policy: policy})
				opOpts.Endpoints = []transport.Endpoint{ep}
				opOpts.WaitToken = func(_ context.Context, h identity.Fingerprint) ([]byte, error) {
					if h != ep.Hub {
						t.Error("signed for wrong hub")
					}
					return []byte(fmt.Sprint(seq.Add(1))), nil
				}
			}
			opConn, acc, err := New(opOpts)
			if err != nil {
				t.Fatal(err)
			}
			defer acc.Close()
			accepted := asyncAccept(t, ctx, acc)
			if !direct {
				waitSignal(t, ctx, registered)
				opConn.Refresh()
				waitSignal(t, ctx, registered)
				if seq.Load() < 2 {
					t.Fatal("refresh reused ticket")
				}
			}
			c, _, err := New(Options{Local: client, Wrap: func(ctx context.Context, conn net.Conn, clientRole bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
				if !expect.IsZero() {
					t.Error("bootstrap pinned the inner identity")
				}
				return secure.WrapStream(ctx, conn, secure.Opts{Local: client, IsClient: clientRole, Expect: expect, Path: p})
			}})
			if err != nil {
				t.Fatal(err)
			}
			result := make(chan acceptedResult, 1)
			go func() {
				s, h, err := c.DialBootstrap(ctx, ep.Addr)
				if err == nil && h != ep.Hub {
					t.Error("wrong bootstrap hub")
				}
				result <- acceptedResult{s, err}
			}()
			if !direct {
				waitSignal(t, ctx, firstRead)
				opConn.Refresh()
				close(resume)
			}
			dial, server := receive(t, result), receive(t, accepted)
			opConn.Refresh()
			kind, via := transport.PathRelay, ep.Addr
			if direct {
				kind, via = transport.PathDirect, ""
			}
			for _, s := range []transport.Session{dial, server} {
				p := s.Path()
				if p.Hub != ep.Hub || p.Kind != kind || p.Via != via {
					t.Fatalf("wrong path %+v", p)
				}
			}
			if dial.Peer() != op.Fingerprint() || server.Peer() != client.Fingerprint() {
				t.Fatal("wrong inner peer")
			}
			type exporter interface{ PairingExporter() [32]byte }
			if dial.(exporter).PairingExporter() != server.(exporter).PairingExporter() {
				t.Fatal("pairing exporter mismatch")
			}
			transfer(t, dial, server)
			if !direct {
				second := asyncAccept(t, ctx, acc)
				conn, err := router.Connect(ctx, ep.Addr, client, ep.Hub, router.Request{Role: router.RoleDial, Target: ep.Hub})
				if conn != nil {
					_ = conn.Close()
				}
				if !errors.Is(err, router.ErrPeerBusy) {
					t.Fatalf("second dial during session: %v", err)
				}
				last := seq.Load()
				_ = dial.Close(nil)
				_ = server.Close(nil)
				waitSignal(t, ctx, registered)
				if seq.Load() <= last {
					t.Fatal("next session reused wait registration")
				}
				_ = acc.Close()
				if r := <-second; r.err == nil {
					t.Fatal("idle wait survived Close")
				}
			}
		})
	}
}

func waitSignal(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWaitBackoff(t *testing.T) {
	local := id(t)
	for _, tc := range []struct {
		err    error
		delays []time.Duration
	}{
		{router.ErrUnauthorized, []time.Duration{30 * time.Second, 30 * time.Second}},
		{router.ErrPeerBusy, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond}},
		{net.ErrClosed, []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond, 1600 * time.Millisecond, 3200 * time.Millisecond, 5 * time.Second, 5 * time.Second}},
	} {
		t.Run(tc.err.Error(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				attempts := make(chan time.Time)
				ep := transport.Endpoint{Kind: "hub", Hub: local.Fingerprint()}
				c, acc, err := New(Options{Local: local, Wrap: stubWrap(ep.Hub), Endpoints: []transport.Endpoint{ep}, WaitToken: func(ctx context.Context, _ identity.Fingerprint) ([]byte, error) {
					select {
					case attempts <- time.Now():
					case <-ctx.Done():
					}
					return nil, tc.err
				}})
				if err != nil {
					t.Fatal(err)
				}
				pending := asyncAccept(t, t.Context(), acc)
				last := <-attempts
				for _, want := range tc.delays {
					now := <-attempts
					if got := now.Sub(last); got != want {
						t.Fatalf("backoff = %v, want %v", got, want)
					}
					last = now
				}
				synctest.Wait()
				c.Refresh()
				if now := <-attempts; now != last {
					t.Fatal("refresh did not interrupt backoff")
				}
				go c.Refresh()
				_ = c.Close()
				if r := <-pending; !errors.Is(r.err, context.Canceled) {
					t.Fatal(r.err)
				}
			})
		})
	}
}

func TestWaitHubMismatchStopsAccept(t *testing.T) {
	local := id(t)
	ep := startHub(t, id(t), nil)
	bad := ep
	bad.Hub = local.Fingerprint()
	_, acc := build(t, Options{Local: local, Wrap: stubWrap(local.Fingerprint()), Endpoints: []transport.Endpoint{ep, bad}})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	if _, err := acc.Accept(ctx); !errors.Is(err, identity.ErrFingerprintMismatch) {
		t.Fatalf("expected terminal hub mismatch, got %v", err)
	}
}

func TestOnRegisterDoesNotBlockRetry(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	local := id(t)
	ep := startHub(t, id(t), router.NewEmbedded())
	token := []byte("wait")
	events := make(chan error)
	release := make(chan struct{})
	defer close(release)
	c, acc, err := New(Options{Local: local, Wrap: stubWrap(ep.Hub), Endpoints: []transport.Endpoint{ep},
		WaitToken: func(context.Context, identity.Fingerprint) ([]byte, error) { return token, nil },
		OnRegister: func(h identity.Fingerprint, tok []byte, err error) {
			if h != ep.Hub || string(tok) != "wait" {
				err = errors.New("incorrect registration")
			}
			if len(tok) > 0 {
				tok[0] = 0
			}
			select {
			case events <- err:
			case <-ctx.Done():
			}
			<-release
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer acc.Close()
	pending := asyncAccept(t, ctx, acc)
	for range 2 {
		select {
		case err := <-events:
			if !errors.Is(err, router.ErrUnauthorized) {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		c.Refresh()
	}
	cancel()
	<-pending
}
