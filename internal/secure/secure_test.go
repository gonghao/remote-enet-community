package secure

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

func local(t *testing.T) *identity.Local {
	t.Helper()
	l, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return l
}
func deadline(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func pair(t *testing.T, zero bool) (transport.Session, transport.Session) {
	t.Helper()
	ctx := deadline(t)
	a, b := local(t), local(t)
	x, y := net.Pipe()
	type result struct {
		s   transport.Session
		err error
	}
	ch := make(chan result, 1)
	opts := Opts{Local: b, Expect: a.Fingerprint(), KeepAlive: 10 * time.Millisecond, Path: transport.PathInfo{Kind: transport.PathRelay, Via: "test"}}
	if zero {
		opts.Expect = identity.Fingerprint{}
	}
	go func() { s, err := WrapStream(ctx, y, opts); ch <- result{s, err} }()
	c, err := WrapStream(ctx, x, Opts{Local: a, Expect: b.Fingerprint(), IsClient: true, KeepAlive: 10 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	t.Cleanup(func() { c.Close(nil); r.s.Close(nil) })
	if c.Peer() != b.Fingerprint() || r.s.Peer() != a.Fingerprint() {
		t.Fatal("wrong peer")
	}
	return c, r.s
}

func TestStreams(t *testing.T) {
	for _, zero := range []bool{false, true} {
		a, b := pair(t, zero)
		ctx := deadline(t)
		if b.Path().Kind != transport.PathRelay || b.Path().Via != "test" {
			t.Fatal("path lost")
		}
		var wg sync.WaitGroup
		for _, side := range [][2]transport.Session{{a, b}, {b, a}} {
			for range 12 {
				wg.Add(2)
				go func() {
					defer wg.Done()
					s, err := side[0].OpenStream(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					defer s.Close()
					if _, err = s.Write([]byte("request")); err != nil {
						t.Error(err)
						return
					}
					if err = s.CloseWrite(); err != nil {
						t.Error(err)
						return
					}
					data, err := io.ReadAll(s)
					if err != nil || string(data) != "response" {
						t.Errorf("response %q: %v", data, err)
					}
				}()
				go func() {
					defer wg.Done()
					s, err := side[1].AcceptStream(ctx)
					if err != nil {
						t.Error(err)
						return
					}
					defer s.Close()
					data, err := io.ReadAll(s)
					if err != nil || string(data) != "request" {
						t.Errorf("request %q: %v", data, err)
						return
					}
					if _, err = s.Write([]byte("response")); err != nil {
						t.Error(err)
					}
					s.CloseWrite()
				}()
			}
		}
		wg.Wait()
		// Zero can be a real clock-quantized RTT. Only a successful periodic
		// Ping can replace this sentinel, so zero still proves sampling ran.
		a.(*session).rtt.Store(-1)
		timer := time.NewTicker(time.Millisecond)
		rtt := a.Stats().RTT
		for rtt < 0 {
			select {
			case <-ctx.Done():
				t.Fatalf("no RTT sample: context=%v, local=%v, peer=%v", ctx.Err(), a.Err(), b.Err())
			case <-a.Done():
				t.Fatalf("local session closed before RTT sample: %v", a.Err())
			case <-b.Done():
				t.Fatalf("peer session closed before RTT sample: %v", b.Err())
			case <-timer.C:
			}
			rtt = a.Stats().RTT
		}
		timer.Stop()
		t.Logf("periodic Ping sample: RTT=%v, zero fingerprint expectation=%t", rtt, zero)
		reason := errors.New("test close")
		a.Close(reason)
		<-a.Done()
		if a.Err() != reason {
			t.Fatal(a.Err())
		}
		select {
		case <-b.Done():
			if b.Err() == nil {
				t.Fatal("missing remote error")
			}
		case <-ctx.Done():
			t.Fatal("remote did not close")
		}
	}
}

func TestCancellation(t *testing.T) {
	a, b := pair(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.OpenStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := a.AcceptStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := b.AcceptStream(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestHandshakeCancellation(t *testing.T) {
	x, y := net.Pipe()
	defer y.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := WrapStream(ctx, x, Opts{Local: local(t), IsClient: true}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestLifetimeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(deadline(t))
	defer cancel()
	x, y := net.Pipe()
	a, b := local(t), local(t)
	ch := make(chan transport.Session, 1)
	go func() { s, _ := WrapStream(ctx, y, Opts{Local: b}); ch <- s }()
	s, err := WrapStream(ctx, x, Opts{Local: a, IsClient: true})
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r == nil {
		t.Fatal("server failed")
	}
	t.Cleanup(func() { s.Close(nil); r.Close(nil) })
	cancel()
	select {
	case <-s.Done():
	case <-time.After(time.Second):
		t.Fatal("cancellation blocked")
	}
	<-r.Done()
	for _, side := range []transport.Session{s, r} {
		if !errors.Is(side.Err(), context.Canceled) {
			t.Fatal(side.Err())
		}
	}
}

func TestRejectedHandshake(t *testing.T) {
	for _, which := range []string{"client-pin", "server-pin", "ecdsa-server", "ecdsa-client", "expired", "bad-signature", "alpn", "missing-alpn", "tls12", "no-client-cert"} {
		t.Run(which, func(t *testing.T) {
			ctx := deadline(t)
			a, b := local(t), local(t)
			x, y := net.Pipe()
			client := Opts{Local: a, Expect: b.Fingerprint(), IsClient: true}
			server := Opts{Local: b, Expect: a.Fingerprint()}
			if which == "client-pin" {
				client.Expect = a.Fingerprint()
			}
			if which == "server-pin" {
				server.Expect = b.Fingerprint()
			}
			if which == "alpn" {
				client.ALPN = "other"
			}
			if which == "ecdsa-server" || which == "ecdsa-client" {
				key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
				if err != nil {
					t.Fatal(err)
				}
				tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
				der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
				if err != nil {
					t.Fatal(err)
				}
				cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
				if which == "ecdsa-server" {
					b.Cert = cert
					client.Expect = identity.Fingerprint{}
				} else {
					a.Cert = cert
					server.Expect = identity.Fingerprint{}
				}
			}
			if which == "expired" {
				tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: time.Now().Add(-2 * time.Hour), NotAfter: time.Now().Add(-time.Hour)}
				der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, b.Key.Public(), b.Key)
				if err != nil {
					t.Fatal(err)
				}
				b.Cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: b.Key}
			}
			if which == "bad-signature" {
				b.Cert.Certificate[0][len(b.Cert.Certificate[0])-1] ^= 1
			}
			serverErr := make(chan error, 1)
			// For rejected clients, a successful client handshake is permitted by TLS
			// 1.3 before the server's rejection arrives. It must not send mux traffic.
			if which == "server-pin" || which == "ecdsa-client" || which == "no-client-cert" {
				go func() {
					s, err := WrapStream(ctx, y, server)
					if s != nil {
						s.Close(nil)
					}
					serverErr <- err
				}()
				certs := []tls.Certificate{a.Cert}
				if which == "no-client-cert" {
					certs = nil
				}
				tc := tls.Client(x, &tls.Config{MinVersion: tls.VersionTLS13, InsecureSkipVerify: true, Certificates: certs, NextProtos: []string{"renet/1"}})
				tc.HandshakeContext(ctx)
				tc.Read(make([]byte, 1))
				x.Close()
				if err := <-serverErr; err == nil {
					t.Fatal("server accepted invalid client")
				}
				return
			}
			// Observe the server's TLS plaintext writes: no application data is ever
			// sent by the rejecting client, and no yamux session is returned.
			go func() {
				cfg := &tls.Config{Certificates: []tls.Certificate{b.Cert}, MinVersion: tls.VersionTLS13, NextProtos: []string{"renet/1"}}
				if which == "missing-alpn" {
					cfg.NextProtos = nil
				}
				if which == "tls12" {
					cfg.MinVersion = tls.VersionTLS12
					cfg.MaxVersion = tls.VersionTLS12
				}
				tc := tls.Server(y, cfg)
				err := tc.HandshakeContext(ctx)
				if err == nil {
					buf := make([]byte, 64)
					n, _ := tc.Read(buf)
					if n > 0 {
						t.Errorf("received %d application bytes", n)
					}
				}
				y.Close()
				serverErr <- err
			}()
			s, err := WrapStream(ctx, x, client)
			if s != nil {
				s.Close(nil)
			}
			if err == nil {
				t.Fatal("accepted invalid server")
			}
			<-serverErr
		})
	}
}

func TestMutualRejection(t *testing.T) {
	for _, rejectClient := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
		a, b := local(t), local(t)
		x, y := net.Pipe()
		client := Opts{Local: a, Expect: b.Fingerprint(), IsClient: true}
		server := Opts{Local: b, Expect: a.Fingerprint()}
		if rejectClient {
			server.Expect = b.Fingerprint()
		} else {
			client.Expect = a.Fingerprint()
		}
		ch := make(chan error, 1)
		go func() {
			s, err := WrapStream(ctx, y, server)
			if s != nil {
				s.Close(nil)
			}
			ch <- err
		}()
		s, err := WrapStream(ctx, x, client)
		if s != nil || err == nil {
			t.Fatal("client returned an application session for rejected handshake")
		}
		if err := <-ch; err == nil {
			t.Fatal("server returned an application session for rejected handshake")
		}
		cancel()
	}
}

func TestBlockedOpenCancellation(t *testing.T) {
	a, _ := pair(t, false)
	s := a.(*session)
	s.openGate <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := s.OpenStream(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	<-s.openGate
}

func TestNoApplicationDataBeforeAcceptance(t *testing.T) {
	ctx := deadline(t)
	a, b := local(t), local(t)
	x, y := net.Pipe()
	ch := make(chan error, 1)
	go func() {
		tc := tls.Server(y, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{b.Cert}, ClientAuth: tls.RequireAnyClientCert, NextProtos: []string{"renet/1"}})
		defer y.Close()
		if err := tc.HandshakeContext(ctx); err != nil {
			ch <- err
			return
		}
		tc.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
		var buf [64]byte
		n, err := tc.Read(buf[:])
		if n != 0 {
			t.Errorf("client sent %d application bytes before server acceptance", n)
		}
		if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
			t.Errorf("expected read timeout, got %v", err)
		}
		ch <- nil
	}()
	s, err := WrapStream(ctx, x, Opts{Local: a, Expect: b.Fingerprint(), IsClient: true, KeepAlive: time.Millisecond})
	if s != nil || err == nil {
		t.Fatal("client started yamux before acceptance")
	}
	if err := <-ch; err != nil {
		t.Fatal(err)
	}
}
