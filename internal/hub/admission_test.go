package hub

import (
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestSetupAdmission(t *testing.T) {
	a := newSetupAdmission(SetupLimits{})
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

func TestEmbeddedSetupSlots(t *testing.T) {
	h, c := local(t), local(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	firstAccepted := make(chan struct{})
	e := NewEmbedded()
	startOnListener(t, Options{Local: h, Embedded: e, SetupLimits: SetupLimits{Concurrent: 1, Timeout: 250 * time.Millisecond}},
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
	if conn, _, err := ConnectBootstrap(t.Context(), addr, c); err == nil {
		conn.Close()
		t.Fatal("accepted overlapping setup")
	}
	stalled.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := stalled.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("stalled setup: %v", err)
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
	case <-e.notify:
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
