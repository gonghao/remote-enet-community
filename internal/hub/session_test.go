package hub

import (
	"crypto/tls"
	"errors"
	"net"
	"runtime"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/identity"
)

func TestCenterRoutes(t *testing.T) {
	before := runtime.NumGoroutine()
	t.Cleanup(func() { eventually(t, func() bool { return runtime.NumGoroutine() <= before }) })
	h, op, client := local(t), local(t), local(t)
	p := testPolicy{admit: func(_ identity.Fingerprint, req Request, now time.Time) (Grant, error) {
		return Grant{Session: SessionID{req.Token[0]}, JTI: [16]byte{req.Token[1]}, Expires: now.Add(time.Minute)}, nil
	}}
	s, addr, _ := start(t, Options{Local: h, Policy: p})
	wait := Request{Role: RoleWait, Target: client.Fingerprint(), Token: []byte{1, 1}}
	dial := Request{Role: RoleDial, Target: op.Fingerprint(), Token: []byte{1, 2}}
	deny := func(dev *identity.Local, req Request, want error) {
		t.Helper()
		c, err := Connect(t.Context(), addr, dev, h.Fingerprint(), req)
		if c != nil {
			c.Close()
		}
		if !errors.Is(err, want) {
			t.Fatalf("token %x: got %v, want %v", req.Token, err, want)
		}
	}
	deny(client, dial, ErrUnknownPeer)
	a := connect(t, addr, h, op, wait)
	deny(op, wait, ErrUnauthorized)
	wait.Token = []byte{1, 3}
	deny(op, wait, ErrPeerBusy)
	other := dial
	other.Token = []byte{2, 4}
	deny(client, other, ErrUnauthorized)
	s.SetAdmitting(false)
	deny(client, dial, ErrUnauthorized)
	s.SetAdmitting(true)
	b := connect(t, addr, h, client, dial)
	deny(client, dial, ErrUnauthorized)
	dial.Token = []byte{1, 5}
	deny(client, dial, ErrPeerBusy)
	s.SetAdmitting(false)
	exchange(t, b, a)
	exchange(t, a, b)
	const total = 2 * 7 * 20000
	b.Close()
	assertClosed(t, a)
	eventually(t, func() bool { return len(s.Usage()) == 0 })
	s.SetAdmitting(true)
	a = connect(t, addr, h, op, wait)
	b = connect(t, addr, h, client, dial)
	u := s.Usage()
	if len(u) != 1 || u[0].Conns != 2 || !u[0].Paired || u[0].RxBytes != total || u[0].TxBytes != total {
		t.Fatalf("reconnected usage %+v", u)
	}
	final := s.Revoke(SessionID{1})
	assertClosed(t, a)
	assertClosed(t, b)
	if final.Conns != 0 || final.Paired || final.PairedFor <= 0 || final.RxBytes != total || final.TxBytes != total || s.Revoke(SessionID{1}) != final || len(s.Usage()) != 0 {
		t.Fatalf("final usage %+v", final)
	}
	wait.Token = []byte{1, 6}
	deny(op, wait, ErrUnauthorized)
	// A cross-session denial left this dial's JTI available.
	wait.Token = []byte{2, 7}
	a = connect(t, addr, h, op, wait)
	b = connect(t, addr, h, client, other)
	exchange(t, a, b)
	a.Close()
	assertClosed(t, b)
	eventually(t, func() bool { return len(s.Usage()) == 0 })
	for _, revoke := range []bool{false, true} {
		if revoke {
			if tail := s.Revoke(SessionID{2}); tail.RxBytes != total/2 || tail.TxBytes != total/2 {
				t.Fatalf("disconnected tail %+v", tail)
			}
		}
		if rx, tx := s.TrafficTotals(); rx != total+total/2 || tx != rx {
			t.Fatalf("node totals %d/%d", rx, tx)
		}
	}
}

func pipeConnection(t *testing.T) *connection {
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	return &connection{Conn: a, raw: a, done: make(chan struct{})}
}

func TestCenterDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := NewServer(Options{Policy: testPolicy{}, SessionRetention: time.Second, SealTTL: time.Second})
		defer s.stopSessionTimers()
		start := time.Now()
		sid := SessionID{1}
		op, client := identity.Fingerprint{1}, identity.Fingerprint{2}
		grant := Grant{Session: sid, JTI: [16]byte{1}, Expires: start.Add(time.Second)}
		wait := Request{Role: RoleWait, Target: client}
		route := func(dev identity.Fingerprint, req Request, g Grant, want byte) *connection {
			t.Helper()
			c := pipeConnection(t)
			if _, st := s.register(c, dev, req, g, true, false, time.Second); st != want {
				t.Fatalf("commit status %d, want %d", st, want)
			}
			return c
		}
		a := route(op, wait, grant, statusOK)
		grant.JTI[0]++
		grant.Expires = start.Add(2 * time.Second)
		b := route(client, Request{Role: RoleDial, Target: op}, grant, statusOK)
		advance := func(d time.Duration) { time.Sleep(d); synctest.Wait() }
		advance(time.Second)
		if u := s.Usage(); len(u) != 1 || u[0].PairedFor != time.Second {
			t.Fatalf("extended ticket deadline: %+v", u)
		}
		if !s.Renew(sid, start.Add(5*time.Second)) {
			t.Fatal("renew failed")
		}
		advance(time.Second)
		if len(s.Usage()) != 1 {
			t.Fatal("old deadline fired")
		}
		s.Renew(sid, start.Add(3*time.Second))
		advance(time.Second)
		for _, c := range []*connection{a, b} {
			select {
			case <-c.done:
			default:
				t.Fatal("route survived shortened deadline")
			}
		}
		if len(s.Usage()) != 0 {
			t.Fatal("expired session still active")
		}
		s.SetSessionRetention(time.Hour)
		advance(time.Second)
		if s.sessions[sid] == nil {
			t.Fatal("old retention timer retired idle record")
		}
		s.SetSessionRetention(time.Second)
		synctest.Wait()
		if s.Renew(sid, time.Now()) {
			t.Fatal("idle record survived retention")
		}

		// The original JTI is reusable only after its inclusive expiry.
		grant.JTI = [16]byte{1}
		grant.Expires = time.Now().Add(time.Second)
		a = route(op, wait, grant, statusOK)
		advance(time.Second)
		select {
		case <-a.done:
		default:
			t.Fatal("wait survived unrenewed ticket deadline")
		}
		grant.Expires = time.Now().Add(time.Second)
		route(op, wait, grant, statusUnauthorized)
		advance(time.Nanosecond)
		route(op, wait, grant, statusOK)
		if s.CloseAll() != 1 || s.CloseAll() != 0 {
			t.Fatal("incorrect disconnected session count")
		}
		grant.JTI[0]++
		route(op, wait, grant, statusOK)
		s.Revoke(sid)
		if s.Renew(sid, time.Now()) {
			t.Fatal("renewed sealed session")
		}
		unknown := SessionID{3}
		if u := s.Revoke(unknown); u != (Usage{Session: unknown}) {
			t.Fatalf("unknown revoke: %+v", u)
		}
		grant.Session = unknown
		grant.JTI[0]++
		grant.Expires = time.Now().Add(time.Hour)
		route(op, wait, grant, statusUnauthorized)
		advance(time.Second + time.Nanosecond)
		route(op, wait, grant, statusOK)
		s.CloseAll()
	})
}

func TestCenterCommitOrdering(t *testing.T) {
	// The policy has already admitted the grant. Hold the routing lock while
	// register queues, then advance the commit clock past the ticket boundary.
	now := time.Unix(100, 0)
	s := NewServer(Options{Policy: testPolicy{}, Now: func() time.Time { return now }})
	sid := SessionID{1}
	dev := identity.Fingerprint{1}
	req := Request{Role: RoleWait, Target: identity.Fingerprint{2}}
	g := Grant{Session: sid, JTI: [16]byte{1}, Expires: now}
	c := pipeConnection(t)
	queued := make(chan struct{})
	result := make(chan byte, 1)
	s.mu.Lock()
	go func() {
		close(queued)
		_, st := s.register(c, dev, req, g, true, false, time.Second)
		result <- st
	}()
	<-queued
	now = now.Add(time.Nanosecond)
	s.mu.Unlock()
	if st := <-result; st != statusUnauthorized {
		t.Fatalf("stale grant committed: %d", st)
	}

	// Either serial order is valid, but no route may survive a concurrent seal.
	for i := range 64 {
		g.Session = SessionID{byte(i + 1)}
		g.JTI = [16]byte{byte(i + 1)}
		g.Expires = now.Add(time.Hour)
		c := pipeConnection(t)
		start := make(chan struct{})
		sealed := make(chan struct{})
		go func() {
			<-start
			_, st := s.register(c, dev, req, g, true, false, time.Second)
			result <- st
		}()
		go func() { <-start; s.Revoke(g.Session); close(sealed) }()
		close(start)
		st := <-result
		<-sealed
		_, used := s.consumed.get(g.JTI, now)
		switch st {
		case statusOK:
			if !used {
				t.Fatal("successful commit did not consume ticket")
			}
			select {
			case <-c.done:
			default:
				t.Fatal("revoke missed concurrent connection")
			}
		case statusUnauthorized:
			if used {
				t.Fatal("seal rejection consumed ticket")
			}
		default:
			t.Fatalf("unexpected status %d", st)
		}
		if len(s.Usage()) != 0 {
			t.Fatal("route survived revoke")
		}
	}
}

func TestCenterStatusWriteFailure(t *testing.T) {
	// Force the OK write to fail after the successful route commit.
	h, op := local(t), local(t)
	inValid, resume := make(chan struct{}), make(chan struct{})
	p := testPolicy{
		admit: func(_ identity.Fingerprint, _ Request, now time.Time) (Grant, error) {
			return Grant{Session: SessionID{1}, JTI: [16]byte{1}, Expires: now.Add(time.Minute)}, nil
		},
		valid: func(Grant, identity.Fingerprint) bool { close(inValid); <-resume; return true },
	}
	s := NewServer(Options{Policy: p, Local: h})
	a, b := net.Pipe()
	defer b.Close()
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	serverConn := &connection{Conn: tls.Server(a, serverTLS(h)), raw: a, done: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer serverConn.Close()
		s.handle(t.Context(), serverConn, time.Second)
	}()
	client := tls.Client(b, &tls.Config{
		Certificates: []tls.Certificate{op.Cert}, NextProtos: []string{ALPN}, MinVersion: tls.VersionTLS13,
		InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error { return verifyPeer(cs, h.Fingerprint()) },
	})
	if err := client.HandshakeContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := writeRequest(client, Request{Role: RoleWait, Target: identity.Fingerprint{2}}); err != nil {
		t.Fatal(err)
	}
	<-inValid
	b.Close()
	close(resume)
	<-done
	if _, ok := s.consumed.get([16]byte{1}, time.Now()); !ok || len(s.Usage()) != 0 {
		t.Fatal("failed OK rolled back consumption or leaked a route")
	}
	s.stopSessionTimers()
}

func TestZeroSessionGrantByMode(t *testing.T) {
	for _, center := range []bool{false, true} {
		s := NewServer(Options{Policy: testPolicy{}, CenterMode: center})
		_, status := s.register(pipeConnection(t), identity.Fingerprint{1}, Request{Role: RoleWait}, Grant{}, true, false, time.Second)
		want := byte(statusOK)
		if center {
			want = statusUnauthorized
		}
		if status != want {
			t.Fatalf("center=%t: status %d, want %d", center, status, want)
		}
	}
}
