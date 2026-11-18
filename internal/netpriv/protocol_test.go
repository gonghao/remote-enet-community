package netpriv

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/packetio"
)

var testLocal = netip.MustParsePrefix("192.0.2.1/32")
var testTarget = netip.MustParseAddr("198.51.100.2")

type fakeDevice struct {
	packetio.Device
	err error
}

func (f *fakeDevice) Name() string        { return "test" }
func (f *fakeDevice) Local() netip.Prefix { return testLocal }
func (f *fakeDevice) Close() error        { return errors.Join(f.Device.Close(), f.err) }
func packet(src, dst netip.Addr) []byte {
	p := make([]byte, 40)
	p[0] = 0x45
	p[8] = 64
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:], 40)
	a, b := src.As4(), dst.As4()
	copy(p[12:], a[:])
	copy(p[16:], b[:])
	p[32] = 0x50
	binary.BigEndian.PutUint16(p[10:], checksum(p[:20], 0))
	sum := uint32(26)
	for i := 12; i < 20; i += 2 {
		sum += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	binary.BigEndian.PutUint16(p[36:], checksum(p[20:], sum))
	return p
}
func session(t *testing.T, cleanup error) (*Device, packetio.Device, <-chan error) {
	t.Helper()
	a, b, e := packetio.NewPair(maxPacket, 1)
	if e != nil {
		t.Fatal(e)
	}
	c, s := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), s, func(context.Context, []netip.Addr) (ownedDevice, error) {
			return &fakeDevice{Device: a, err: cleanup}, nil
		})
	}()
	d, e := handshake(context.Background(), c, testTarget)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = d.Close(); _ = b.Close() })
	return d, b, done
}
func TestSessionRoundTripAndClose(t *testing.T) {
	d, b, done := session(t, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	p := packet(testTarget, testLocal.Addr())
	if _, e := d.WritePacket(ctx, p); e != nil {
		t.Fatal(e)
	}
	buf := make([]byte, maxPacket)
	if n, e := b.ReadPacket(ctx, buf); e != nil || n != 40 {
		t.Fatalf("read %d %v", n, e)
	}
	if _, e := b.WritePacket(ctx, packet(testLocal.Addr(), testTarget)); e != nil {
		t.Fatal(e)
	}
	if n, e := d.ReadPacket(ctx, buf); e != nil || n != 40 {
		t.Fatalf("read %d %v", n, e)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			if e := d.Close(); e != nil {
				t.Error(e)
			}
		})
	}
	wg.Wait()
	if e := <-done; e != nil {
		t.Fatal(e)
	}
}
func TestCleanupFailurePropagates(t *testing.T) {
	d, _, done := session(t, errors.New("secret cleanup details"))
	if e := d.Close(); e == nil || e.Error() != "netpriv: helper cleanup failed" {
		t.Fatalf("close %v", e)
	}
	if e := <-done; e == nil {
		t.Fatal("lost server cleanup failure")
	}
}
func TestCancellationDoesNotDestroyDevice(t *testing.T) {
	d, b, _ := session(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := d.ReadPacket(ctx, make([]byte, 1500)); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := d.WritePacket(ctx, packet(testTarget, testLocal.Addr())); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e := b.WritePacket(context.Background(), packet(testLocal.Addr(), testTarget)); e != nil {
		t.Fatal(e)
	}
	if _, e := d.ReadPacket(context.Background(), make([]byte, 1500)); e != nil {
		t.Fatal(e)
	}
}
func TestCloseWithUnreadPackets(t *testing.T) {
	d, b, done := session(t, nil)
	for range 4 {
		if _, e := b.WritePacket(context.Background(), packet(testLocal.Addr(), testTarget)); e != nil {
			t.Fatal(e)
		}
	}
	if e := d.Close(); e != nil {
		t.Fatal(e)
	}
	<-done
}
func TestDisconnectDuringSetupCancels(t *testing.T) {
	c, s := net.Pipe()
	entered := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), s, func(ctx context.Context, _ []netip.Addr) (ownedDevice, error) {
			close(entered)
			<-ctx.Done()
			return nil, ctx.Err()
		})
	}()
	a := testTarget.As4()
	if e := (&wire{Conn: c}).send('H', a[:]); e != nil {
		t.Fatal(e)
	}
	<-entered
	_ = c.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("setup leaked")
	}
}
func TestMalformedHandshakeNeverOpens(t *testing.T) {
	for _, tc := range []struct {
		k byte
		p []byte
	}{{'H', nil}, {'H', []byte{127, 0, 0, 1}}, {'P', []byte{1, 2, 3, 4}}} {
		c, s := net.Pipe()
		done := make(chan error, 1)
		go func() {
			done <- serve(context.Background(), s, func(context.Context, []netip.Addr) (ownedDevice, error) {
				t.Error("opened invalid request")
				return nil, io.EOF
			})
		}()
		_ = (&wire{Conn: c}).send(tc.k, tc.p)
		if e := <-done; e == nil {
			t.Fatal("accepted invalid handshake")
		}
		_ = c.Close()
	}
}
func TestPacketBoundary(t *testing.T) {
	p := packet(testTarget, testLocal.Addr())
	if !allowedPacket(p, testTarget, testLocal.Addr()) {
		t.Fatal("valid packet rejected")
	}
	for _, edit := range []func([]byte){func(p []byte) { p[0] = 0x46 }, func(p []byte) { p[9] = 17 }, func(p []byte) { p[2] = 1 }, func(p []byte) { p[6] = 0x20 }, func(p []byte) { p[7] = 1 }, func(p []byte) { p[12] = 1 }, func(p []byte) { p[16] = 1 }, func(p []byte) { p[32] = 0xf0 }} {
		b := append([]byte(nil), p...)
		edit(b)
		if allowedPacket(b, testTarget, testLocal.Addr()) {
			t.Fatal("accepted invalid packet")
		}
	}
}
func TestOversizedFrame(t *testing.T) {
	c, s := net.Pipe()
	defer c.Close()
	defer s.Close()
	go func() { _, _ = c.Write([]byte{'H', 255, 255}) }()
	if _, _, e := (&wire{Conn: s}).receive(); e == nil {
		t.Fatal("accepted oversized frame")
	}
}
func TestInvalidOpenReturnsNil(t *testing.T) {
	for _, targets := range [][]netip.Addr{nil, {netip.MustParseAddr("127.0.0.1")}, {testTarget, testTarget}} {
		d, e := Open(context.Background(), targets, Options{})
		if d != nil || e == nil {
			t.Fatalf("invalid open %v %v", d, e)
		}
	}
}

func TestDisconnectAfterCreationCleansUp(t *testing.T) {
	d, _, done := session(t, nil)
	stream := d.Device.(*streamDevice)
	_ = stream.w.Close()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("helper leaked after IPC disconnect")
	}
	if e := d.Close(); e == nil {
		t.Fatal("disconnect lost cleanup uncertainty")
	}
}
func TestResourceContextCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, s := net.Pipe()
	a, b, _ := packetio.NewPair(maxPacket, 1)
	defer b.Close()
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), s, func(context.Context, []netip.Addr) (ownedDevice, error) { return &fakeDevice{Device: a}, nil })
	}()
	d, e := handshake(ctx, c, testTarget)
	if e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("resource context leaked")
	}
	if e = d.Close(); e != nil {
		t.Fatal(e)
	}
}

func TestDefaultHelperInvocationIsInert(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"--ipc", "/tmp/secret"}, {"--ipc", "/tmp/secret", "--uid", "0"}} {
		if e := RunHelper(context.Background(), args); e == nil {
			t.Fatal("accepted invalid invocation")
		}
	}
}
func TestAuthorizationQuoting(t *testing.T) {
	s := authorizationScript("/Applications/A'B\\C\"/renet-net-helper", "/private/tmp/renet-net-test/ipc", 501)
	expected := "do shell script \"exec '/Applications/A'\\\"'\\\"'B\\\\C\\\"/renet-net-helper' --ipc '/private/tmp/renet-net-test/ipc' --uid 501\" with administrator privileges"
	if s != expected {
		t.Fatalf("unexpected quoting: %s", s)
	}
}

func TestHandshakeCancellationReturnsContextError(t *testing.T) {
	c, s := net.Pipe()
	defer s.Close()
	defer c.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		d, e := handshake(ctx, c, testTarget)
		if d != nil {
			t.Error("returned device after cancellation")
		}
		done <- e
	}()
	cancel()
	select {
	case e := <-done:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("handshake cancellation leaked")
	}
}

// controlledWriter gives the test explicit ownership of every native write completion.
type controlledWriter struct {
	*fakeDevice
	started chan struct{}
	release chan error
	peer    packetio.Device
}

func (d *controlledWriter) WritePacket(ctx context.Context, p []byte) (int, error) {
	select {
	case d.started <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	}
	select {
	case e := <-d.release:
		if e != nil {
			return 0, e
		}
		return len(p), nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}
func slowSession(t *testing.T) (*Device, *controlledWriter, <-chan error) {
	t.Helper()
	a, b, e := packetio.NewPair(maxPacket, 1)
	if e != nil {
		t.Fatal(e)
	}
	native := &controlledWriter{fakeDevice: &fakeDevice{Device: a}, started: make(chan struct{}, 1), release: make(chan error), peer: b}
	c, s := net.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- serve(context.Background(), s, func(context.Context, []netip.Addr) (ownedDevice, error) { return native, nil })
	}()
	d, e := handshake(context.Background(), c, testTarget)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = d.Close(); _ = b.Close() })
	return d, native, done
}
func TestSlowNativeWriteAcknowledgmentAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, native, _ := slowSession(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		first := make(chan error, 1)
		go func() { _, e := d.WritePacket(ctx, packet(testTarget, testLocal.Addr())); first <- e }()
		<-native.started
		synctest.Wait()
		select {
		case e := <-first:
			t.Fatalf("write returned before native completion: %v", e)
		default:
		}
		cancel()
		if e := <-first; !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
		results := make(chan error, 1)
		go func() {
			for range 4 {
				if _, e := d.WritePacket(context.Background(), packet(testTarget, testLocal.Addr())); e != nil {
					results <- e
					return
				}
			}
			results <- nil
		}()
		synctest.Wait()
		select {
		case <-native.started:
			t.Fatal("next packet overtook canceled write confirmation")
		default:
		}
		native.release <- nil
		for range 4 {
			select {
			case <-native.started:
			case e := <-results:
				t.Fatalf("burst failed before native write: %v", e)
			}
			synctest.Wait()
			select {
			case e := <-results:
				t.Fatalf("burst returned before native completion: %v", e)
			default:
			}
			native.release <- nil
		}
		if e := <-results; e != nil {
			t.Fatal(e)
		}
		if e := d.Close(); e != nil {
			t.Fatal(e)
		}
	})
}

func TestSlowNativeBurstBackpressure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, native, done := slowSession(t)
		results := make(chan error, 1)
		go func() {
			for range 8 {
				if _, e := d.WritePacket(context.Background(), packet(testTarget, testLocal.Addr())); e != nil {
					results <- e
					return
				}
			}
			results <- nil
		}()
		<-native.started
		synctest.Wait()
		select {
		case e := <-done:
			t.Fatalf("healthy burst closed behind slow native writer: %v", e)
		default:
		}
		select {
		case e := <-results:
			t.Fatalf("burst bypassed native backpressure: %v", e)
		default:
		}
		native.release <- nil
		for range 7 {
			<-native.started
			native.release <- nil
		}
		if e := <-results; e != nil {
			t.Fatal(e)
		}
		if e := d.Close(); e != nil {
			t.Fatal(e)
		}
		if e := <-done; e != nil {
			t.Fatal(e)
		}
	})
}
func TestSlowNativeCloseAndDisconnect(t *testing.T) {
	for _, disconnect := range []bool{false, true} {
		t.Run(map[bool]string{false: "close", true: "disconnect"}[disconnect], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				d, native, done := slowSession(t)
				result := make(chan error, 1)
				go func() { _, e := d.WritePacket(context.Background(), packet(testTarget, testLocal.Addr())); result <- e }()
				<-native.started
				synctest.Wait()
				if disconnect {
					_ = d.Device.(*streamDevice).w.Close()
				}
				e := d.Close()
				if disconnect && e == nil {
					t.Fatal("missing cleanup uncertainty")
				}
				if !disconnect && e != nil {
					t.Fatal(e)
				}
				if e := <-result; e == nil {
					t.Fatal("uncompleted native write succeeded")
				}
				<-done
			})
		})
	}
}
func TestNativeWriteFailureIsNotNormalClose(t *testing.T) {
	d, native, done := slowSession(t)
	result := make(chan error, 1)
	go func() { _, e := d.WritePacket(context.Background(), packet(testTarget, testLocal.Addr())); result <- e }()
	<-native.started
	native.release <- errors.New("secret native failure")
	if e := <-result; e == nil {
		t.Fatal("native failure published write success")
	}
	if e := d.Close(); e == nil || e.Error() != "netpriv: helper network I/O failed" {
		t.Fatalf("close %v", e)
	}
	if e := <-done; e == nil {
		t.Fatal("missing server I/O failure")
	}
}

func TestUnreadPacketsDoNotBlockWriteConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, native, _ := slowSession(t)
		for range queueLimit + 1 {
			if _, e := native.peer.WritePacket(context.Background(), packet(testLocal.Addr(), testTarget)); e != nil {
				t.Fatal(e)
			}
			synctest.Wait()
		}
		stream := d.Device.(*streamDevice)
		if cap(stream.reads) != queueLimit || len(stream.reads) != queueLimit {
			t.Fatalf("read queue budget: length=%d capacity=%d", len(stream.reads), cap(stream.reads))
		}
		result := make(chan error, 1)
		go func() { _, e := d.WritePacket(context.Background(), packet(testTarget, testLocal.Addr())); result <- e }()
		<-native.started
		native.release <- nil
		if e := <-result; e != nil {
			t.Fatal(e)
		}
		if e := d.Close(); e != nil {
			t.Fatal(e)
		}
	})
}
