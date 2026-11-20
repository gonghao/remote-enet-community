package hubauth

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
)

func TestStandaloneReload(t *testing.T) {
	f := setup(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var seconds atomic.Int64
	seconds.Store(f.now.Unix())
	p := f.policy(t)
	server := hub.NewServer(hub.Options{Local: f.h, Policy: p, Now: func() time.Time { return time.Unix(seconds.Load(), 0) }})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("server shutdown timed out")
		}
	})
	connect := func(local *identity.Local, req hub.Request, want error) net.Conn {
		t.Helper()
		c, err := hub.Connect(ctx, ln.Addr().String(), local, f.h.Fingerprint(), req)
		if !errors.Is(err, want) {
			t.Fatalf("connect: got %v, want %v", err, want)
		}
		if c != nil {
			t.Cleanup(func() { c.Close() })
		}
		return c
	}
	assertClosed := func(conns ...net.Conn) {
		t.Helper()
		for _, c := range conns {
			if err := c.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
				t.Fatal(err)
			}
			var timeout net.Error
			if _, err := c.Read(make([]byte, 1)); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
				t.Fatalf("expected connection closure, got %v", err)
			}
		}
	}
	wait := f.claims(hub.RoleWait)
	wait.PairUntil = f.now.Add(time.Minute)
	register := hub.Request{Role: hub.RoleWait, Token: f.sign(t, wait)}
	dial := hub.Request{Role: hub.RoleDial, Target: f.op.Fingerprint(), Token: f.sign(t, f.claims(hub.RoleDial))}
	f.cfg.Deny.Devices = []string{f.client.Fingerprint().String()}
	revoked := f.policy(t)
	for range 2 {
		w := connect(f.op, register, nil)
		d := connect(f.client, dial, nil)
		if waits, pairs := server.SetPolicy(revoked); waits != 0 || pairs != 1 {
			t.Fatalf("closed %d waits and %d pairs", waits, pairs)
		}
		assertClosed(w, d)
		connect(f.client, dial, hub.ErrUnauthorized)
		server.SetPolicy(p)
	}
	w := connect(f.op, register, nil)
	entry := hub.Request{Role: hub.RoleDial, Target: f.h.Fingerprint()}
	seconds.Store(wait.PairUntil.Unix() + 31)
	connect(f.client, entry, hub.ErrUnauthorized)
	seconds.Store(wait.PairUntil.Unix() + 30)
	d := connect(f.client, entry, nil)
	f.cfg.Deny.Devices = nil
	f.cfg.PairingOperator = f.client.Fingerprint().String()
	f.cfg.Issuers = append(f.cfg.Issuers, Issuer{Operator: f.cfg.PairingOperator, PublicKey: base64.RawURLEncoding.EncodeToString(f.client.Key.Public().(ed25519.PublicKey))})
	other := f.policy(t)
	if waits, pairs := server.SetPolicy(other); waits != 0 || pairs != 1 {
		t.Fatalf("operator change closed %d waits and %d pairs", waits, pairs)
	}
	assertClosed(w, d)
	server.SetPolicy(p)
	w = connect(f.op, register, nil)
	if waits, pairs := server.SetPolicy(other); waits != 1 || pairs != 0 {
		t.Fatalf("operator change closed %d waits and %d pairs", waits, pairs)
	}
	assertClosed(w)
}
