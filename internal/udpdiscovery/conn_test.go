package udpdiscovery

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"
)

func listenNative(t *testing.T, ctx context.Context) *Conn {
	t.Helper()
	c, err := Listen(ctx, netip.MustParseAddr("127.0.0.1"), 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func nativeTool(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestNativeUnicastMetadataAndSourcePort(t *testing.T) {
	c := listenNative(t, t.Context())
	tool := nativeTool(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var b [128]byte
	if n, err := c.Write(ctx, nil, tool.LocalAddr().(*net.UDPAddr)); n != 0 || err == nil {
		t.Fatalf("empty reply must fail without sending a dummy byte: n=%d err=%v", n, err)
	}
	for _, request := range [][]byte{{0, 0xff, 0x11, 1}, {0}} {
		if _, err := tool.WriteToUDP(request, c.LocalAddr()); err != nil {
			t.Fatal(err)
		}
		n, peer, err := c.Read(ctx, b[:])
		if err != nil || !bytes.Equal(b[:n], request) || peer.String() != tool.LocalAddr().String() {
			t.Fatalf("unicast receive: n=%d peer=%v err=%v", n, peer, err)
		}
		if n, err := c.Write(ctx, request, peer); n != len(request) || err != nil {
			t.Fatalf("reply: n=%d want=%d err=%v", n, len(request), err)
		}
		n, from, err := tool.ReadFromUDP(b[:])
		if err != nil || !bytes.Equal(b[:n], request) || from.String() != c.LocalAddr().String() {
			t.Fatalf("fixed reply source/port: n=%d from=%v err=%v", n, from, err)
		}
	}
	address := c.LocalAddr()
	address.IP[0] = 99
	if c.LocalAddr().IP.String() != "127.0.0.1" {
		t.Fatal("LocalAddr shared mutable IP storage")
	}
}

func TestNativeCanceledReadCanReuseSocket(t *testing.T) {
	c := listenNative(t, t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var b [64]byte
	if _, _, err := c.Read(ctx, b[:]); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("read deadline: %v", err)
	}
	tool := nativeTool(t)
	if _, err := tool.WriteToUDP([]byte("after cancel"), c.LocalAddr()); err != nil {
		t.Fatal(err)
	}
	next, done := context.WithTimeout(t.Context(), time.Second)
	defer done()
	n, _, err := c.Read(next, b[:])
	if err != nil || string(b[:n]) != "after cancel" {
		t.Fatalf("socket unusable after canceled operation: n=%d err=%v", n, err)
	}
}

type pendingDeadline struct {
	context.Context
	when time.Time
}

func (c pendingDeadline) Deadline() (time.Time, bool) { return c.when, true }

func TestNativeSocketDeadlineBeforeContextNotification(t *testing.T) {
	c := listenNative(t, t.Context())
	// Model the interval in which the socket clock has expired but the context
	// timer has not yet scheduled its notification. No sleep is needed.
	ctx := pendingDeadline{Context: context.Background(), when: time.Now().Add(-time.Second)}
	if _, _, err := c.Read(ctx, make([]byte, 10)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired socket deadline lost context cause: %v", err)
	}
	ioFailure := errors.New("underlying socket failure")
	if err := c.ioError(ctx, ioFailure); !errors.Is(err, ioFailure) {
		t.Fatalf("deadline masked a non-timeout I/O error: %v", err)
	}
}

func TestNativeWaitingReaderCanBeCanceled(t *testing.T) {
	c := listenNative(t, t.Context())
	firstCtx, stopFirst := context.WithCancel(t.Context())
	defer stopFirst()
	first := make(chan error, 1)
	go func() { _, _, err := c.Read(firstCtx, make([]byte, 10)); first <- err }()
	// Acquiring the occupied gate below would fail if both operations were
	// allowed into the shared read deadline at once.
	limit := time.Now().Add(time.Second)
	for len(c.read) == 0 {
		if time.Now().After(limit) {
			t.Fatal("first reader did not acquire the gate")
		}
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	if _, _, err := c.Read(ctx, make([]byte, 10)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("queued reader cancellation: %v", err)
	}
	stopFirst()
	select {
	case err := <-first:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("first reader survived cancellation")
	}
}

func TestNativeCloseAndOwnerCancellation(t *testing.T) {
	for _, ownerCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "owner"}[ownerCancel], func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			c := listenNative(t, ctx)
			done := make(chan error, 1)
			go func() { _, _, err := c.Read(t.Context(), make([]byte, 10)); done <- err }()
			want := net.ErrClosed
			if ownerCancel {
				want = context.Canceled
				cancel()
			} else if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("blocked read termination: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("blocked read survived termination")
			}
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNativeTruncatedDatagramDiscarded(t *testing.T) {
	c := listenNative(t, t.Context())
	tool := nativeTool(t)
	for _, b := range [][]byte{bytes.Repeat([]byte{0xff}, 128), []byte("ok")} {
		if _, err := tool.WriteToUDP(b, c.LocalAddr()); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var b [8]byte
	n, _, err := c.Read(ctx, b[:])
	if err != nil || string(b[:n]) != "ok" {
		t.Fatalf("truncated payload escaped: n=%d err=%v", n, err)
	}
}

func TestNativeValidationAndOperationCancellation(t *testing.T) {
	for _, addr := range []string{"0.0.0.0", "255.255.255.255", "224.0.0.1", "::1", "192.0.2.2"} {
		if c, err := Listen(t.Context(), netip.MustParseAddr(addr), 0); err == nil {
			c.Close()
			t.Errorf("invalid/unassigned bind accepted: %s", addr)
		}
	}
	if c, err := Listen(t.Context(), netip.MustParseAddr("127.0.0.1"), -1); err == nil {
		c.Close()
		t.Fatal("invalid port accepted")
	}
	c := listenNative(t, t.Context())
	for _, peer := range []*net.UDPAddr{nil, {IP: net.IPv4zero, Port: 1234}, {IP: net.IPv4(127, 0, 0, 1)}, {IP: net.ParseIP("::1"), Port: 1234}, {IP: net.IPv4(224, 0, 0, 1), Port: 1234}, {IP: net.IPv4bcast, Port: 1234}, {IP: net.IPv4(127, 255, 255, 255), Port: 1234}} {
		if _, err := c.Write(t.Context(), nil, peer); err == nil {
			t.Errorf("invalid reply peer accepted: %v", peer)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := c.Read(ctx, make([]byte, 10)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := c.Write(ctx, []byte("x"), &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, _, err := c.Read(t.Context(), nil); err == nil {
		t.Fatal("empty receive buffer accepted")
	}
}
