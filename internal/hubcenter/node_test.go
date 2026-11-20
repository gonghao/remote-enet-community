package hubcenter

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json/v2"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/ticket"
)

const activatePath = "/v1/node/activate"
const heartbeatPath = "/v1/node/heartbeat"

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixture struct {
	t *testing.T
	*Node
	fc                  *fakecenter.Center
	clock               atomic.Int64
	addr                string
	op, peer, signer    *identity.Local
	id                  center.NodeID
	paths, keys, bodies []string
	reports             []center.Heartbeat
	latency             time.Duration
}

func local(t *testing.T) *identity.Local {
	t.Helper()
	l, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func (f *fixture) now() time.Time          { return time.Unix(0, f.clock.Load()) }
func (f *fixture) advance(d time.Duration) { f.clock.Add(int64(d)) }

func newFixture(t *testing.T, after func(time.Duration, func()) func() bool) *fixture {
	t.Helper()
	f := &fixture{t: t, op: local(t), peer: local(t), signer: local(t)}
	f.clock.Store(time.Now().Truncate(time.Second).UnixNano())
	f.fc = fakecenter.New(t, fakecenter.Options{CenterID: "c", Now: f.now})
	h := local(t)
	cfg := testConfig()
	cfg.Center.API = f.fc.Entries()
	id := f.fc.RegisterNode(fakecenter.NodeConfig{FP: h.Fingerprint(), Addr: "127.0.0.1", Port: 9200})
	if err := f.id.UnmarshalText([]byte(id)); err != nil {
		t.Fatal(err)
	}
	cfg.NodeID = f.id
	cfg.TicketKeys = []ed25519.PublicKey{f.fc.TicketKey(), f.signer.Key.Public().(ed25519.PublicKey)}
	p, err := NewPolicy(cfg, h.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	s := hub.NewServer(hub.Options{Local: h, Policy: p, Now: f.now, AfterFunc: after})
	// fakecenter exposes accepted seq and kids, but not raw retry keys,
	// heartbeat bodies or response transit time. Observe its real TLS traffic.
	client := *f.fc.HTTPClient()
	base := client.Transport
	client.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
		f.paths = append(f.paths, r.URL.Path)
		if r.Body != nil {
			data, err := io.ReadAll(r.Body)
			r.Body.Close()
			if err != nil {
				return nil, err
			}
			r.Body = io.NopCloser(bytes.NewReader(data))
			switch r.URL.Path {
			case activatePath:
				f.keys = append(f.keys, r.Header.Get("Idempotency-Key"))
				f.bodies = append(f.bodies, string(data))
			case heartbeatPath:
				var hb center.Heartbeat
				if err := json.Unmarshal(data, &hb); err != nil {
					return nil, err
				}
				f.reports = append(f.reports, hb)
			}
		}
		res, err := base.RoundTrip(r)
		if r.URL.Path == heartbeatPath {
			f.advance(f.latency)
		}
		return res, err
	})
	api, err := center.NewClient(center.ClientOptions{Center: cfg.Center, Role: "hub", NodeKey: h.Key, Node: f.id, HTTP: &client, Now: f.now})
	if err != nil {
		t.Fatal(err)
	}
	f.Node = NewNode(NodeOptions{Config: cfg, Local: h, Server: s, Policy: p, API: api, Version: "dev", Now: f.now})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return f
}

// Each step runs between completed API calls, on the test goroutine.
func (f *fixture) run(steps ...func(time.Duration)) error {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Second)
	defer cancel()
	f.wait = func(ctx context.Context, d time.Duration) error {
		if len(steps) == 0 {
			cancel()
			return ctx.Err()
		}
		step := steps[0]
		steps = steps[1:]
		step(d)
		return ctx.Err()
	}
	err := f.Run(ctx)
	if len(steps) != 0 {
		f.t.Fatalf("%d unused steps: %v", len(steps), err)
	}
	return err
}

func (f *fixture) token(gen uint32, sid, jti byte) hub.Request {
	f.t.Helper()
	now := f.now()
	c := ticket.CenterClaims{Center: "c", Node: f.id, Hub: f.o.Local.Fingerprint(), Generation: gen, Device: f.op.Fingerprint(), Peer: f.peer.Fingerprint(), Role: uint8(hub.RoleWait), Session: [16]byte{sid}, JTI: [16]byte{jti}, IssuedAt: now, NotBefore: now}
	raw, err := ticket.SignCenter(f.signer.Key, c, ticket.CenterIssuePolicy{TicketTTL: 120, TicketMaxTTL: 300, ClockSkew: 30, HardDeadline: now.Add(time.Hour)})
	if err != nil {
		f.t.Fatal(err)
	}
	return hub.Request{Role: hub.RoleWait, Target: c.Peer, Token: raw}
}
func (f *fixture) connect(dev *identity.Local, r hub.Request, want error) net.Conn {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 3*time.Second)
	defer cancel()
	c, err := hub.Connect(ctx, f.addr, dev, f.o.Local.Fingerprint(), r)
	if !errors.Is(err, want) {
		f.t.Fatalf("connect: got %v, want %v", err, want)
	}
	if c != nil {
		f.t.Cleanup(func() { c.Close() })
	}
	return c
}

func TestActivationRetries(t *testing.T) {
	for _, rotate := range []bool{false, true} {
		t.Run(map[bool]string{false: "same key", true: "23 hours"}[rotate], func(t *testing.T) {
			f := newFixture(t, nil)
			// A valid policy alone cannot bypass the pre-listen gate.
			f.o.Policy.SetGeneration(1)
			f.connect(f.op, f.token(1, 1, 1), hub.ErrUnauthorized)
			for range 3 {
				f.fc.Inject(0, activatePath, fakecenter.Fault{DropResponse: true})
			}
			err := f.run(func(d time.Duration) {
				if d != time.Second || len(f.keys) != 3 {
					t.Fatal(d, f.keys)
				}
				cfg := f.o.Config
				cfg.TicketKeys = cfg.TicketKeys[:1]
				if err := f.Reload(cfg); err != nil {
					t.Fatal(err)
				}
				if rotate {
					f.advance(23 * time.Hour)
				}
			}, func(d time.Duration) {
				if d != 15*time.Second {
					t.Fatal(d)
				}
			}, func(time.Duration) {})
			if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			gen, seq, kids := f.fc.NodeStatus(f.id.String())
			if f.paths[0] != "/v1/meta" || len(f.keys) != 4 || f.keys[0] == "" || f.keys[0] != f.keys[2] || (f.keys[0] != f.keys[3]) != rotate || (f.bodies[0] != f.bodies[3]) != rotate || gen != map[bool]uint32{false: 1, true: 2}[rotate] || seq != 2 || len(kids) != 1 {
				t.Fatal(f.keys, gen, seq, kids)
			}
		})
	}
}

func TestActivationTerminal(t *testing.T) {
	for _, code := range []string{"bad_request", "upgrade_required", "unauthenticated", "node_unknown", "node_key_mismatch", "node_check_failed", "idempotency_key_reused", "node_superseded"} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t, nil)
			f.fc.Inject(0, activatePath, fakecenter.Fault{Code: code})
			err := f.run()
			if (code == "node_superseded" && !errors.Is(err, ErrSuperseded)) || (code != "node_superseded" && errorCode(err) != code) || len(f.keys) != 1 || len(f.reports) != 0 {
				t.Fatal(err, f.paths)
			}
		})
	}
}

func TestSkewAndHeartbeatRecovery(t *testing.T) {
	f := newFixture(t, nil)
	f.fc.Inject(0, activatePath, fakecenter.Fault{Code: "clock_skew", TimeOffset: time.Minute * 2})
	f.fc.Inject(0, activatePath, fakecenter.Fault{TimeOffset: 32 * time.Second})
	f.run(func(d time.Duration) {
		if d != 15*time.Second {
			t.Fatal(d)
		}
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 1, 1), hub.ErrUnauthorized)
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 1, 2), nil)
		f.fc.Inject(0, heartbeatPath, fakecenter.Fault{TimeOffset: -31 * time.Second})
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 2, 3), hub.ErrUnauthorized)
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 2, 4), nil)
		f.fc.Inject(0, heartbeatPath, fakecenter.Fault{Code: "node_disabled"})
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 3, 5), hub.ErrUnauthorized)
		if len(f.o.Server.Usage()) != 0 {
			t.Fatal("disabled node retained live routes")
		}
	}, func(time.Duration) {
		f.connect(f.op, f.token(1, 3, 6), nil)
	})
	if f.keys[0] != f.keys[1] {
		t.Fatal("clock skew changed activation key")
	}
}

func TestSuperseded(t *testing.T) {
	f := newFixture(t, nil)
	old := f.token(1, 1, 1)
	err := f.run(func(time.Duration) {
		f.connect(f.op, old, nil)
		o := f.o
		p, err := NewPolicy(o.Config, o.Local.Fingerprint())
		if err != nil {
			t.Fatal(err)
		}
		o.Policy = p
		o.Server = hub.NewServer(hub.Options{Local: o.Local, Policy: p})
		second := NewNode(o)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		second.wait = func(context.Context, time.Duration) error {
			if _, err := p.Admit(f.op.Fingerprint(), old, f.now()); err == nil {
				t.Fatal("accepted old generation")
			}
			if _, err := p.Admit(f.op.Fingerprint(), f.token(2, 1, 2), f.now()); err != nil {
				t.Fatal(err)
			}
			cancel()
			return ctx.Err()
		}
		if err := second.Run(ctx); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
	if !errors.Is(err, ErrSuperseded) || len(f.o.Server.Usage()) != 0 || len(f.keys) != 2 || len(f.reports) != 1 {
		t.Fatal(err, f.paths)
	}
	f.connect(f.op, f.token(1, 2, 3), hub.ErrUnauthorized)
}

func TestClockUncertainty(t *testing.T) {
	f := newFixture(t, nil)
	start := f.now()
	for _, d := range []int{29, 30, -27, -28} {
		got := f.clockOK(t.Context(), start.Add(time.Duration(d)*time.Second), start, start.Add(2*time.Second))
		if got != (d == 29 || d == -27) {
			t.Fatal(d, got)
		}
	}
}

func (f *fixture) route() (center.SessionID, net.Conn, net.Conn, hub.Request) {
	t := f.t
	t.Helper()
	client := func(role string, local *identity.Local) *center.Client {
		c, err := center.NewClient(center.ClientOptions{Center: f.o.Config.Center, Role: role, Device: local.Key, HTTP: f.fc.HTTPClient(), Now: f.now})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	op, peer := client("operator", f.op), client("client", f.peer)
	ctx := t.Context()
	key := center.NewIdempotencyKey
	_, code := f.fc.CreateAccount("test", 1, time.Time{})
	if _, _, err := op.Login(ctx, key(), code, "op"); err != nil {
		t.Fatal(err)
	}
	s, err := op.CreateSession(ctx, key(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.Redeem(ctx, key(), *s.Code); err != nil {
		t.Fatal(err)
	}
	get := func(c *center.Client, role hub.Role) hub.Request {
		v, err := c.Ticket(ctx, key(), s.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return hub.Request{Role: role, Token: v.Raw, Target: v.PeerFP}
	}
	spare := get(op, hub.RoleWait)
	wait := f.connect(f.op, get(op, hub.RoleWait), nil)
	dial := f.connect(f.peer, get(peer, hub.RoleDial), nil)
	if _, err := op.Established(ctx, key(), s.SessionID, f.peer.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	return s.SessionID, wait, dial, spare
}

func TestClosedTailAndLoad(t *testing.T) {
	f := newFixture(t, nil)
	var sid center.SessionID
	var w, d net.Conn
	var spare hub.Request
	f.run(func(time.Duration) {
		sid, w, d, spare = f.route()
		f.advance(2 * time.Second)
	}, func(time.Duration) {
		f.advance(2 * time.Second)
		data := []byte("relay data")
		if err := w.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := d.Write(data); err != nil {
			t.Fatal(err)
		}
		if _, err := io.ReadFull(w, make([]byte, len(data))); err != nil {
			t.Fatal(err)
		}
		d.Close()
		for limit := time.Now().Add(time.Second); len(f.o.Server.Usage()) != 0; {
			if time.Now().After(limit) {
				t.Fatal("route did not disconnect")
			}
			time.Sleep(time.Millisecond)
		}
		f.fc.RevokeSession(sid.String())
	}, func(time.Duration) {
		f.connect(f.op, spare, hub.ErrUnauthorized)
		f.fc.Inject(0, heartbeatPath, fakecenter.Fault{DropResponse: true})
	}, func(time.Duration) {}, func(time.Duration) {})
	r := f.reports
	want := center.ClosedUsage{SessionID: sid, RxBytes: 10, TxBytes: 10, PairedS: 4}
	if len(r) != 5 || r[0].Load.Conns != 2 || r[0].Load.Waiting != 0 || r[0].Sessions[0].State != "paired" || r[1].Load.RxBps != 5 || r[1].Load.TxBps != 5 || len(r[1].Sessions) != 0 || len(r[1].Closed) != 0 || !slices.Equal(r[2].Closed, []center.ClosedUsage{want}) || !slices.Equal(r[2].Closed, r[3].Closed) || r[3].Seq != 4 || len(r[4].Closed) != 0 {
		t.Fatal(r)
	}
	if v := f.fc.Snapshot()[sid.String()]; !v.Released || v.Usage.TX != 10 {
		t.Fatal(v)
	}
}

func TestSendTimeLeaseAndDisconnectedDeadline(t *testing.T) {
	type timer struct {
		delay time.Duration
		fire  func()
	}
	timers := make(chan timer, 16)
	f := newFixture(t, func(delay time.Duration, fn func()) func() bool {
		var stopped atomic.Bool
		timers <- timer{delay, func() {
			if !stopped.Swap(true) {
				fn()
			}
		}}
		return func() bool { return !stopped.Swap(true) }
	})
	var w, d net.Conn
	var due timer
	f.run(func(time.Duration) {
		_, w, d, _ = f.route()
		f.latency = 4 * time.Second
	}, func(remaining time.Duration) {
		if remaining != 11*time.Second {
			t.Fatalf("heartbeat scheduled after %v", remaining)
		}
		// fakecenter grants 300 seconds to an established session. The hub
		// schedules its real D callback with four seconds already consumed.
		for len(timers) > 0 {
			due = <-timers
		}
		if due.delay != 296*time.Second {
			t.Fatalf("renewed timer = %v", due.delay)
		}
		f.latency = 0
		f.fc.Inject(0, heartbeatPath, fakecenter.Fault{Code: "unavailable"})
	}, func(time.Duration) {
		if len(timers) != 0 || len(f.o.Server.Usage()) != 1 {
			t.Fatal("failed heartbeat changed the lease")
		}
		f.advance(due.delay)
		due.fire()
		if len(f.o.Server.Usage()) != 0 {
			t.Fatal("hub still carries expired route")
		}
		for _, c := range []net.Conn{w, d} {
			c.SetReadDeadline(time.Now().Add(time.Second))
			var b [1]byte
			if _, err := c.Read(b[:]); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("expired connection read: %v", err)
			}
		}
	})
}

func TestHeartbeatErrors(t *testing.T) {
	for _, code := range []string{"unauthenticated", "upgrade_required", "node_disabled", "bad_request", "internal", "unavailable", "clock_skew"} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t, nil)
			f.run(func(time.Duration) {
				f.connect(f.op, f.token(1, 1, 1), nil)
				f.fc.Inject(0, heartbeatPath, fakecenter.Fault{Code: code})
			}, func(time.Duration) {
				closes := code == "unauthenticated" || code == "upgrade_required" || code == "node_disabled"
				if (len(f.o.Server.Usage()) == 0) != closes {
					t.Fatal("unexpected route state")
				}
				want := hub.ErrPeerBusy
				if closes || code == "clock_skew" {
					want = hub.ErrUnauthorized
				}
				f.connect(f.op, f.token(1, 2, 2), want)
			})
			if len(f.reports) != 2 || f.reports[1].Seq != 2 {
				t.Fatal(f.reports)
			}
		})
	}
}

func TestLateHeartbeatDiscarded(t *testing.T) {
	f := newFixture(t, nil)
	f.run(func(time.Duration) {
		f.connect(f.op, f.token(1, 1, 1), nil)
		// The successful reply would revoke this unknown session.
		f.latency = HeartbeatTimeout + time.Second
	}, func(time.Duration) {
		if len(f.o.Server.Usage()) != 1 {
			t.Fatal("late revocation applied")
		}
		f.latency = 0
	}, func(time.Duration) {
		if len(f.o.Server.Usage()) != 0 {
			t.Fatal("timely revocation skipped")
		}
	})
}

func TestCancelInflight(t *testing.T) {
	f := newFixture(t, nil)
	f.fc.Inject(0, activatePath, fakecenter.Fault{Delay: 5 * time.Second})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx) }()
	for limit := time.Now().Add(time.Second); f.fc.Calls(0, activatePath) == 0; {
		if time.Now().After(limit) {
			t.Fatal("activation did not arrive")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not join on cancellation")
	}
}

func TestActivationAndHeartbeatRetention(t *testing.T) {
	var scheduled atomic.Int64
	f := newFixture(t, func(d time.Duration, _ func()) func() bool {
		scheduled.Store(int64(d))
		return func() bool { return true }
	})
	f.o.Server.SetSessionRetention(time.Second)
	var expected time.Duration
	check := func() {
		t.Helper()
		if got := time.Duration(scheduled.Load()); got != expected {
			t.Fatalf("cleanup after %v, want %v", got, expected)
		}
	}
	f.run(func(time.Duration) {
		f.connect(f.op, f.token(1, 1, 1), nil)
		expected = time.Duration(scheduled.Load()) + 300*time.Second + ClockSkew
		f.o.Server.CloseAll()
		check()
		f.o.Server.SetSessionRetention(time.Hour)
	}, func(time.Duration) { check() })
}
