package hub

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestSetupAdmission(t *testing.T) {
	a := newSetupAdmission(SetupLimits{}, true)
	now := time.Now()
	for range 16 {
		if !a.acquire(now) {
			t.Fatal("initial burst rejected")
		}
	}
	a.release()
	if a.acquire(now.Add(124 * time.Millisecond)) {
		t.Fatal("token refilled early")
	}
	if !a.acquire(now.Add(125 * time.Millisecond)) {
		t.Fatal("token did not refill")
	}
	if a.acquire(now.Add(time.Second)) {
		t.Fatal("over concurrency limit")
	}
	a.release()
	if !a.acquire(now.Add(time.Second)) {
		t.Fatal("released slot unavailable")
	}
}

func TestSetupSlots(t *testing.T) {
	for _, mode := range []string{"embedded", "standalone"} {
		t.Run(mode, func(t *testing.T) { testSetupSlots(t, mode == "embedded") })
	}
}

func testSetupSlots(t *testing.T, embedded bool) {
	t.Helper()
	h, c := local(t), local(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstAccepted := make(chan struct{})
	o := Options{Local: h, SetupLimits: SetupLimits{Concurrent: 1, Timeout: 250 * time.Millisecond}}
	if embedded {
		o.Embedded = NewEmbedded()
	} else {
		o.Policy = testPolicy{}
	}
	startOnListener(t, o,
		&acceptHookListener{Listener: ln, firstAccepted: firstAccepted})
	addr := ln.Addr().String()
	stalled, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer stalled.Close()
	select {
	case <-firstAccepted:
	case <-time.After(2 * time.Second):
		t.Fatal("silent connection was not accepted")
	}
	if conn, err := Connect(t.Context(), addr, c, h.Fingerprint(), Request{Role: RoleWait}); err == nil || errors.Is(err, ErrUnauthorized) {
		if conn != nil {
			conn.Close()
		}
		t.Fatal("accepted overlapping setup")
	}
	stalled.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := stalled.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("stalled setup: %v", err)
	}
	if !embedded {
		connect(t, addr, h, c, Request{Role: RoleWait})
		// The single setup slot is released after the wait's ok is written, so
		// an immediate dial may still find it occupied and be dropped.
		deadline := time.Now().Add(2 * time.Second)
		for {
			conn, err := Connect(t.Context(), addr, h, h.Fingerprint(), Request{Role: RoleDial, Target: c.Fingerprint()})
			if err == nil {
				conn.Close()
				return
			}
			if errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrUnknownPeer) || errors.Is(err, ErrPeerBusy) || time.Now().After(deadline) {
				t.Fatalf("dial after slot release: %v", err)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	conn, fp, err := ConnectBootstrap(t.Context(), addr, c)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if fp != h.Fingerprint() {
		t.Fatal("wrong bootstrap identity")
	}
	select {
	case <-o.Embedded.notify:
	case <-time.After(2 * time.Second):
		t.Fatal("handoff was not published")
	}
	if _, _, err := ConnectBootstrap(t.Context(), addr, c); !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("handoff retained setup slot: %v", err)
	}
}

type acceptHookListener struct {
	net.Listener
	firstAccepted chan struct{}
}

func (l *acceptHookListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil && l.firstAccepted != nil {
		close(l.firstAccepted)
		l.firstAccepted = nil
	}
	return conn, err
}

func TestEmbeddedHandoffTimeout(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "shorter", timeout: 500 * time.Millisecond},
		{name: "longer", timeout: 12 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h, c := local(t), local(t)
			_, addr, _ := start(t, Options{Local: h, Embedded: NewEmbedded(), SetupLimits: SetupLimits{Timeout: tc.timeout}})
			started := time.Now()
			conn, _, err := ConnectBootstrap(t.Context(), addr, c)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetReadDeadline(time.Now().Add(tc.timeout + 2*time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("pending handoff did not close by the configured timeout: %v", err)
			}
			if elapsed := time.Since(started); elapsed < tc.timeout {
				t.Fatalf("pending handoff closed after %v, want at least %v", elapsed, tc.timeout)
			}
		})
	}
}
