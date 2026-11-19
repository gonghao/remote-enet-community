package hub

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
)

type testPolicy struct {
	admit    func(identity.Fingerprint, Request, time.Time) (Grant, error)
	valid    func(Grant, identity.Fingerprint) bool
	operator identity.Fingerprint
}

func (p testPolicy) Admit(dev identity.Fingerprint, req Request, now time.Time) (Grant, error) {
	if p.admit != nil {
		return p.admit(dev, req, now)
	}
	return Grant{}, nil
}
func (p testPolicy) Valid(g Grant, dev identity.Fingerprint) bool {
	return p.valid == nil || p.valid(g, dev)
}
func (p testPolicy) PairingOperator() identity.Fingerprint { return p.operator }

func TestStandaloneRequiresPolicy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := NewServer(Options{Local: local(t)}).Serve(ctx, ln); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected startup rejection, got %v", err)
	}
}

func TestPairingEntry(t *testing.T) {
	h, op, client := local(t), local(t), local(t)
	deadline := time.Unix(100, 0)
	var seconds atomic.Int64
	seconds.Store(100)
	p := testPolicy{operator: op.Fingerprint(), admit: func(_ identity.Fingerprint, req Request, _ time.Time) (Grant, error) {
		if string(req.Token) == "window" {
			return Grant{PairingUntil: deadline}, nil
		}
		return Grant{}, nil
	}}
	s, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}, Now: func() time.Time { return time.Unix(seconds.Load(), 0) }})
	dial := Request{Role: RoleDial, Target: h.Fingerprint()}
	reject := func(req Request, want error) {
		t.Helper()
		if c, err := Connect(t.Context(), addr, client, h.Fingerprint(), req); !errors.Is(err, want) {
			if c != nil {
				c.Close()
			}
			t.Fatalf("got %v, want %v", err, want)
		}
	}
	reject(dial, ErrUnauthorized)
	s.SetPolicy(p)
	other := local(t)
	connect(t, addr, h, other, Request{Role: RoleWait, Token: []byte("window")})
	reject(dial, ErrUnknownPeer)
	connect(t, addr, h, op, Request{Role: RoleWait})
	reject(dial, ErrUnauthorized)
	s.SetPolicy(nil)
	s.SetPolicy(p)
	connect(t, addr, h, other, Request{Role: RoleWait, Token: []byte("window")})
	wait := connect(t, addr, h, op, Request{Role: RoleWait, Token: []byte("window")})
	reject(Request{Role: RoleDial, Target: h.Fingerprint(), Token: []byte("unexpected")}, ErrUnauthorized)
	seconds.Store(101)
	reject(dial, ErrUnauthorized)
	seconds.Store(100)
	c := connect(t, addr, h, client, dial)
	seconds.Store(101)
	reject(dial, ErrPeerBusy)
	exchange(t, c, wait)
}

func TestReloadRevokesRoutes(t *testing.T) {
	h, client := local(t), local(t)
	expire := time.Now()
	var seconds atomic.Int64
	now := func() time.Time { return expire.Add(time.Duration(seconds.Load()) * time.Second) }
	p := testPolicy{admit: func(_ identity.Fingerprint, req Request, _ time.Time) (Grant, error) {
		return Grant{Ref: string(req.Token), Expires: expire}, nil
	}}
	s, addr, _ := start(t, Options{Local: h, Policy: p, Now: now})
	idle, keepIdle, op, keepOp := local(t), local(t), local(t), local(t)
	wait := func(id *identity.Local, token string) net.Conn {
		return connect(t, addr, h, id, Request{Role: RoleWait, Token: []byte(token)})
	}
	dial := func(id *identity.Local, token string) net.Conn {
		return connect(t, addr, h, client, Request{Role: RoleDial, Target: id.Fingerprint(), Token: []byte(token)})
	}
	badWait := wait(idle, "revoked")
	goodIdle := wait(keepIdle, "valid")
	badPair, badDial := wait(op, "valid"), dial(op, "revoked")
	goodPair, goodDial := wait(keepOp, "valid"), dial(keepOp, "valid")
	seconds.Store(1)
	p.valid = func(g Grant, _ identity.Fingerprint) bool { return g.Ref != "revoked" }
	if w, d := s.SetPolicy(p); w != 1 || d != 1 {
		t.Fatalf("closed %d waits, %d pairs", w, d)
	}
	for _, c := range []net.Conn{badWait, badPair, badDial} {
		assertClosed(t, c)
	}
	exchange(t, goodPair, goodDial)
	s.SetPolicy(testPolicy{})
	exchange(t, goodIdle, dial(keepIdle, "valid"))
}

func assertClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	var timeout net.Error
	if _, err := c.Read(make([]byte, 1)); err == nil || (errors.As(err, &timeout) && timeout.Timeout()) {
		t.Fatalf("expected closed connection: %v", err)
	}
}

func TestAdmissionRecheck(t *testing.T) {
	for _, tc := range []string{"reload", "expire", "registered"} {
		t.Run(tc, func(t *testing.T) {
			h, op := local(t), local(t)
			entered, resume := make(chan struct{}), make(chan struct{})
			deadline := time.Now()
			var seconds atomic.Int64
			now := func() time.Time { return deadline.Add(time.Duration(seconds.Load()) * time.Second) }
			p := testPolicy{admit: func(_ identity.Fingerprint, _ Request, _ time.Time) (Grant, error) {
				if tc != "registered" {
					close(entered)
					<-resume
				}
				return Grant{Expires: deadline}, nil
			}}
			if tc == "registered" {
				p.valid = func(Grant, identity.Fingerprint) bool {
					close(entered)
					<-resume
					return true
				}
			}
			deny := testPolicy{valid: func(Grant, identity.Fingerprint) bool { return false }}
			s, addr, _ := start(t, Options{Local: h, Policy: p, Now: now})
			type result struct {
				c   net.Conn
				err error
			}
			done := make(chan result, 1)
			go func() {
				c, err := Connect(t.Context(), addr, op, h.Fingerprint(), Request{Role: RoleWait})
				done <- result{c, err}
			}()
			select {
			case <-entered:
			case <-time.After(3 * time.Second):
				close(resume)
				t.Fatal("admission did not reach barrier")
			}
			revoked := make(chan int, 1)
			switch tc {
			case "registered":
				started := make(chan struct{})
				go func() {
					close(started)
					waits, _ := s.SetPolicy(deny)
					revoked <- waits
				}()
				<-started
			case "reload":
				s.SetPolicy(deny)
			case "expire":
				seconds.Store(1)
			}
			close(resume)
			r := <-done
			if tc == "registered" {
				if count := <-revoked; count != 1 {
					t.Fatalf("revoked %d registered waits", count)
				}
				// Reload can close the registered socket before its OK is written.
				if r.err == nil {
					defer r.c.Close()
					assertClosed(t, r.c)
				} else if errors.Is(r.err, ErrUnauthorized) {
					t.Fatal("admission denied after commit")
				}
			} else if !errors.Is(r.err, ErrUnauthorized) {
				t.Fatalf("stale grant: %v", r.err)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.waits) != 0 {
				t.Fatal("stale wait in routing index")
			}
		})
	}
}
