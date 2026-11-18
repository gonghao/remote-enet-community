package carside

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

func forwarder(t *testing.T, nic NIC) *Forwarder {
	t.Helper()
	f, err := NewForwarder(nic, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestTargetBoundary(t *testing.T) {
	f := forwarder(t, testNIC())
	for _, target := range []string{
		"localhost:6801", "127.0.0.2:0", "127.0.0.1:6801", "127.0.0.0:6801", "127.255.255.255:6801",
		"0.0.0.0:6801", "255.255.255.255:6801", "224.0.0.1:6801", "192.168.1.2:6801", "[::1]:6801", "[::ffff:127.0.0.2]:6801", "127.0.0.2:-1", "127.0.0.2:65536", "127.0.0.2", "",
	} {
		t.Run(target, func(t *testing.T) {
			if _, err := f.target(proto.OpenTunnel{Kind: "tcp", Target: target}); err == nil {
				t.Fatal("invalid target accepted")
			}
		})
	}
	if got, err := f.target(proto.OpenTunnel{Kind: "tcp", Target: "127.0.0.2:22"}); err != nil || got.String() != "127.0.0.2:22" {
		t.Fatalf("allowed target = %v, %v", got, err)
	}
	if _, err := f.target(proto.OpenTunnel{Kind: "reverse", Target: "127.0.0.2:6801"}); err == nil {
		t.Fatal("reverse target accepted")
	}
	// Every real interface address must be rejected, even when a broad prefix
	// would otherwise contain it. Prefix.Contains alone is not sufficient.
	addresses, err := localIPv4Addresses()
	if err != nil {
		t.Fatal(err)
	}
	f.nic.Prefix = netip.MustParsePrefix("0.0.0.0/0")
	for ip := range addresses {
		if _, err := f.target(proto.OpenTunnel{Kind: "tcp", Target: net.JoinHostPort(ip.String(), "22")}); err == nil {
			t.Fatalf("local interface accepted: %s", ip)
		}
	}
	if _, err := NewForwarder(NIC{}, nil); err == nil {
		t.Fatal("invalid NIC accepted")
	}
}

func streamPair(t *testing.T, ctx context.Context, a, b transport.Session) (transport.Stream, transport.Stream) {
	t.Helper()
	client, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	server, err := b.AcceptStream(ctx)
	if err != nil {
		client.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

func listenTCP(t *testing.T) *net.TCPListener {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	if err := ln.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return ln
}

func TestForwarderConcurrentOpaqueHalfClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	f := forwarder(t, testNIC())
	ln := listenTCP(t)
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	const n = 10
	echoErrors := make(chan error, n)
	go func() {
		var wg sync.WaitGroup
		defer wg.Wait()
		for range n {
			conn, err := ln.AcceptTCP()
			if err != nil {
				echoErrors <- err
				return
			}
			wg.Go(func() {
				defer conn.Close()
				stop := context.AfterFunc(ctx, func() { conn.Close() })
				defer stop()
				if conn.RemoteAddr().(*net.TCPAddr).IP.String() != "127.0.0.1" {
					echoErrors <- fmt.Errorf("wrong source %s", conn.RemoteAddr())
					return
				}
				body, err := io.ReadAll(conn)
				if err == nil {
					_, err = conn.Write(body)
				}
				if err == nil {
					err = conn.CloseWrite()
				}
				echoErrors <- err
			})
		}
	}()
	var clients sync.WaitGroup
	for range n {
		client, server := streamPair(t, ctx, a, b)
		go f.HandleTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: ln.Addr().String()}, server)
		clients.Go(func() {
			defer client.Close()
			payload := bytes.Repeat([]byte{0, 0xff, 0x13, 0xf4, 0x42}, 70000)
			if _, err := client.Write(payload); err != nil {
				t.Error(err)
				return
			}
			if err := client.CloseWrite(); err != nil {
				t.Error(err)
				return
			}
			got, err := io.ReadAll(client)
			if err != nil || !bytes.Equal(got, payload) {
				t.Errorf("opaque reply: bytes=%d err=%v", len(got), err)
			}
		})
	}
	clients.Wait()
	for range n {
		select {
		case err := <-echoErrors:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestForwarderCancellationAndCloseJoin(t *testing.T) {
	for _, closeForwarder := range []bool{false, true} {
		t.Run(fmt.Sprint(closeForwarder), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			f := forwarder(t, testNIC())
			ln := listenTCP(t)
			a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
			client, server := streamPair(t, ctx, a, b)
			done := make(chan struct{})
			go func() {
				f.HandleTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: ln.Addr().String()}, server)
				close(done)
			}()
			conn, err := ln.AcceptTCP()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if closeForwarder {
				if err := f.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("handler survived cancellation")
			}
			if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := conn.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("vehicle connection not closed: %v", err)
			}
			if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("tunnel not closed: %v", err)
			}
		})
	}
}

func TestRejectedAndClosedHandlersCloseStreams(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	f := forwarder(t, testNIC())
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	for _, closed := range []bool{false, true} {
		if closed {
			f.Close()
		}
		client, server := streamPair(t, ctx, a, b)
		f.HandleTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: "localhost:6801"}, server)
		if _, err := client.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("rejected stream remained open: %v", err)
		}
	}
}
