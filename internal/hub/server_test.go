package hub

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
)

func local(t *testing.T) *identity.Local {
	t.Helper()
	l, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func start(t *testing.T, o Options) (*Server, string, context.CancelFunc) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s, cancel := startOnListener(t, o, ln)
	return s, ln.Addr().String(), cancel
}

func startOnListener(t *testing.T, o Options, ln net.Listener) (*Server, context.CancelFunc) {
	t.Helper()
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(t.Context())
	s := NewServer(o)
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("Serve: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("Serve leaked handlers on shutdown")
		}
	})
	return s, cancel
}

func connect(t *testing.T, addr string, hub, device *identity.Local, req Request) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	c, err := Connect(ctx, addr, device, hub.Fingerprint(), req)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func exchange(t *testing.T, a, b net.Conn) {
	t.Helper()
	// Includes every byte value; the relay must treat all bytes as opaque.
	want := bytes.Repeat([]byte{0, 255, 22, 3, 0, 1, 128}, 20000)
	errCh := make(chan error, 1)
	go func() { errCh <- writeAll(a, want) }()
	got := make([]byte, len(want))
	if _, err := io.ReadFull(b, got); err != nil {
		t.Fatal(err)
	}
	if err := <-errCh; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("relay changed bytes")
	}
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("state did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestRelay(t *testing.T) {
	h, a, b := local(t), local(t), local(t)
	s, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
	dialRequest := Request{Role: RoleDial, Target: b.Fingerprint(), Token: []byte("reusable-device-ticket")}
	wait := connect(t, addr, h, b, Request{Role: RoleWait})
	if _, err := Connect(t.Context(), addr, b, h.Fingerprint(), Request{Role: RoleWait}); !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("duplicate waiter: %v", err)
	}
	dial := connect(t, addr, h, a, dialRequest)
	if _, err := Connect(t.Context(), addr, a, h.Fingerprint(), dialRequest); !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("second dial: %v", err)
	}
	var wg sync.WaitGroup
	wg.Go(func() { exchange(t, dial, wait) })
	wg.Go(func() { exchange(t, wait, dial) })
	wg.Wait()
	_ = dial.Close()
	if _, err := wait.Read(make([]byte, 1)); err == nil {
		t.Fatal("wait side survived dial close")
	}
	eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.waits) == 0
	})
	wait = connect(t, addr, h, b, Request{Role: RoleWait})
	dial = connect(t, addr, h, a, dialRequest)
	_ = wait.Close()
	if _, err := dial.Read(make([]byte, 1)); err == nil {
		t.Fatal("dial side survived wait close")
	}
}

func TestUnknownAndAuthorization(t *testing.T) {
	h, a, b := local(t), local(t), local(t)
	_, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
	if _, err := Connect(t.Context(), addr, a, h.Fingerprint(), Request{Role: RoleDial, Target: b.Fingerprint()}); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("unknown: %v", err)
	}
	_, restricted, _ := start(t, Options{Local: h, Policy: testPolicy{valid: func(_ Grant, dev identity.Fingerprint) bool {
		return dev == b.Fingerprint()
	}}})
	if _, err := Connect(t.Context(), restricted, a, h.Fingerprint(), Request{Role: RoleWait}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized: %v", err)
	}
	_, tokenAddr, _ := start(t, Options{Local: h, Policy: testPolicy{admit: func(_ identity.Fingerprint, req Request, _ time.Time) (Grant, error) {
		if string(req.Token) != "secret" {
			return Grant{}, ErrUnauthorized
		}
		return Grant{}, nil
	}}})
	if _, err := Connect(t.Context(), tokenAddr, b, h.Fingerprint(), Request{Role: RoleWait, Token: []byte("wrong")}); !errors.Is(err, ErrUnauthorized) {
		t.Fatal(err)
	}
	connect(t, tokenAddr, h, b, Request{Role: RoleWait, Target: a.Fingerprint(), Token: []byte("secret")})
	if _, err := Connect(t.Context(), tokenAddr, h, h.Fingerprint(), Request{Role: RoleDial, Target: b.Fingerprint(), Token: []byte("secret")}); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("restricted wait target: %v", err)
	}
	connect(t, tokenAddr, h, a, Request{Role: RoleDial, Target: b.Fingerprint(), Token: []byte("secret")})
}

func TestPairCompetition(t *testing.T) {
	h, a, b := local(t), local(t), local(t)
	_, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
	connect(t, addr, h, b, Request{Role: RoleWait})
	const n = 24
	results := make(chan error, n)
	var winner net.Conn
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			c, err := Connect(t.Context(), addr, a, h.Fingerprint(), Request{Role: RoleDial, Target: b.Fingerprint()})
			if err == nil {
				mu.Lock()
				winner = c
				mu.Unlock()
			}
			results <- err
		})
	}
	wg.Wait()
	close(results)
	defer func() {
		if winner != nil {
			_ = winner.Close()
		}
	}()
	wins := 0
	for err := range results {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrPeerBusy) {
			t.Fatal(err)
		}
	}
	if wins != 1 {
		t.Fatalf("winners = %d", wins)
	}
}

func TestWaitDisconnectAndEagerData(t *testing.T) {
	h, b := local(t), local(t)
	s, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
	for _, eager := range []bool{false, true} {
		c := connect(t, addr, h, b, Request{Role: RoleWait})
		if eager {
			_, _ = c.Write([]byte{1})
		}
		_ = c.Close()
		eventually(t, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.waits) == 0
		})
	}
}

func TestEmbedded(t *testing.T) {
	h, a := local(t), local(t)
	e := NewEmbedded()
	_, addr, cancel := start(t, Options{Local: h, Embedded: e})
	dial := connect(t, addr, h, a, Request{Role: RoleDial, Target: h.Fingerprint(), Token: []byte("ignored")})
	ctx, stop := context.WithTimeout(t.Context(), 3*time.Second)
	defer stop()
	c, fp, err := e.Accept(ctx)
	if err != nil || fp != a.Fingerprint() {
		t.Fatalf("embedded accept: %v, %v", fp, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	exchange(t, dial, c)
	exchange(t, c, dial)
	if _, err := Connect(ctx, addr, a, h.Fingerprint(), Request{Role: RoleDial, Target: h.Fingerprint()}); !errors.Is(err, ErrPeerBusy) {
		t.Fatalf("embedded busy: %v", err)
	}
	_ = c.Close()
	connect(t, addr, h, a, Request{Role: RoleDial, Target: h.Fingerprint()})
	cancel() // Shutdown must also close connections queued for Accept.
}

func TestOuterTLS(t *testing.T) {
	h, a := local(t), local(t)
	_, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
	if _, err := Connect(t.Context(), addr, a, a.Fingerprint(), Request{Role: RoleWait}); err == nil {
		t.Fatal("wrong hub pin accepted")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		certs   []tls.Certificate
		alpn    []string
		version uint16
	}{
		{"non-ed25519", []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, []string{ALPN}, tls.VersionTLS13},
		{"no-certificate", nil, []string{ALPN}, tls.VersionTLS13},
		{"wrong-alpn", []tls.Certificate{a.Cert}, []string{"wrong"}, tls.VersionTLS13},
		{"no-alpn", []tls.Certificate{a.Cert}, nil, tls.VersionTLS13},
		{"tls12", []tls.Certificate{a.Cert}, []string{ALPN}, tls.VersionTLS12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer raw.Close()
			_ = raw.SetDeadline(time.Now().Add(3 * time.Second))
			c := tls.Client(raw, &tls.Config{InsecureSkipVerify: true, Certificates: tc.certs, NextProtos: tc.alpn, MinVersion: tc.version, MaxVersion: tc.version})
			// TLS 1.3 may deliver the server's rejection on the first read.
			if err := c.HandshakeContext(t.Context()); err != nil {
				return
			}
			_ = writeRequest(c, Request{Role: RoleWait})
			if _, err := c.Read(make([]byte, 1)); err == nil {
				t.Fatal("invalid TLS peer accepted")
			}
		})
	}
}

func TestShutdownAndCancellation(t *testing.T) {
	h, a, b := local(t), local(t), local(t)
	_, addr, cancel := start(t, Options{Local: h, Policy: testPolicy{}})
	wait := connect(t, addr, h, b, Request{Role: RoleWait})
	dial := connect(t, addr, h, a, Request{Role: RoleDial, Target: b.Fingerprint()})
	partial, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer partial.Close()
	cancel()
	for _, c := range []net.Conn{wait, dial, partial} {
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection survived shutdown")
		}
	}
	ctx, stop := context.WithCancel(t.Context())
	stop()
	if _, _, err := NewEmbedded().Accept(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := Connect(ctx, addr, a, h.Fingerprint(), Request{Role: RoleWait}); err == nil {
		t.Fatal("canceled Connect succeeded")
	}
}

func TestNoGoroutineLeak(t *testing.T) {
	before := runtime.NumGoroutine()
	for range 5 {
		t.Run("pairing and revocation", func(t *testing.T) {
			h, op, client := local(t), local(t), local(t)
			p := testPolicy{operator: op.Fingerprint(), admit: func(_ identity.Fingerprint, _ Request, now time.Time) (Grant, error) {
				return Grant{PairingUntil: now.Add(time.Minute)}, nil
			}}
			s, addr, _ := start(t, Options{Local: h, Policy: p})
			wait := connect(t, addr, h, op, Request{Role: RoleWait})
			dial := connect(t, addr, h, client, Request{Role: RoleDial, Target: h.Fingerprint()})
			s.SetPolicy(nil)
			assertClosed(t, wait)
			assertClosed(t, dial)
		})
		t.Run("cycle", func(t *testing.T) {
			h, a, b := local(t), local(t), local(t)
			_, addr, _ := start(t, Options{Local: h, Policy: testPolicy{}})
			connect(t, addr, h, b, Request{Role: RoleWait})
			connect(t, addr, h, a, Request{Role: RoleDial, Target: b.Fingerprint()})
		})
	}
	eventually(t, func() bool { return runtime.NumGoroutine() <= before })
}
