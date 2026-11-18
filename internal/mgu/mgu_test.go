package mgu

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

type openFunc func(context.Context, proto.OpenTunnel) (transport.Stream, error)

func (f openFunc) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	return f(ctx, h)
}

type pipeStream struct{ net.Conn }

func (p pipeStream) CloseWrite() error { return p.Close() }

var gateway = netip.MustParseAddrPort("127.0.0.1:6801")

func positive() hsfz.Frame {
	return diag(0x63, 0xF4, []byte{0x62, 0x17, 0x2A, 0, 169, 254, 42, 7, 255, 255, 0, 0, 0, 0, 0, 0})
}
func diag(src, dst byte, p []byte) hsfz.Frame {
	return hsfz.Frame{Control: hsfz.ControlDiag, Body: hsfz.DiagBody{Src: src, Dst: dst, Payload: p}.Marshal()}
}
func frameBytes(f hsfz.Frame) []byte { b, _ := f.MarshalBinary(); return b }
func pipeQuery(t *testing.T, opts Options, serve func(net.Conn) error) (Info, error) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { defer b.Close(); done <- serve(b) }()
	opener := openFunc(func(_ context.Context, h proto.OpenTunnel) (transport.Stream, error) {
		if h.Kind != "tcp" || h.Target != gateway.String() {
			t.Errorf("header: %+v", h)
		}
		return pipeStream{a}, nil
	})
	info, err := Discover(context.Background(), opener, gateway, opts)
	if e := <-done; e != nil {
		t.Fatal(e)
	}
	return info, err
}
func readRequest(c io.Reader, tester byte) error {
	f, e := hsfz.ReadFrame(c, 100)
	if e != nil {
		return e
	}
	want := []byte{tester, 0x63, 0x22, 0x17, 0x2A}
	if f.Control != hsfz.ControlDiag || !bytes.Equal(f.Body, want) {
		return errors.New("incorrect request")
	}
	return nil
}
func TestDiscoverFrames(t *testing.T) {
	info, err := pipeQuery(t, Options{}, func(c net.Conn) error {
		if e := readRequest(c, 0xF4); e != nil {
			return e
		}
		frames := []hsfz.Frame{{Control: hsfz.ControlAck, Body: []byte{0xF4, 0x63, 0x22, 0x17, 0x2A}}, diag(0x62, 0xF4, []byte{0x7F, 0x22, 0x31}), diag(0x63, 0xF3, []byte{0x7F, 0x22, 0x31}), diag(0x63, 0xF4, []byte{0x62, 0x17, 0x2B}), diag(0x63, 0xF4, []byte{0x7F, 0x22, 0x78}), {Control: hsfz.ControlAliveCheck, Body: make([]byte, 32)}}
		var wire []byte
		for _, f := range frames {
			wire = append(wire, frameBytes(f)...)
		}
		// Exercise both fragmented headers/bodies and concatenated frames.
		for _, n := range []int{1, 3, 7, len(wire) - 11} {
			if _, e := c.Write(wire[:n]); e != nil {
				return e
			}
			wire = wire[n:]
		}
		alive, e := hsfz.ReadFrame(c, 100)
		if e != nil {
			return e
		}
		if !bytes.Equal(frameBytes(alive), frameBytes(hsfz.AliveResponse(0xF4))) {
			return errors.New("incorrect alive response")
		}
		return hsfz.WriteFrame(c, positive())
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.Address.String() != "169.254.42.7" || info.Netmask.String() != "255.255.0.0" || info.Third.String() != "0.0.0.0" {
		t.Fatal(info)
	}
}
func TestFailures(t *testing.T) {
	oversized := make([]byte, 6)
	binary.BigEndian.PutUint32(oversized, 4097)
	cases := map[string][]byte{
		"NRC 0x31":           frameBytes(diag(0x63, 0xF4, []byte{0x7F, 0x22, 0x31})),
		"malformed negative": frameBytes(diag(0x63, 0xF4, []byte{0x7F, 0x22})),
		"empty diagnostic":   frameBytes(diag(0x63, 0xF4, nil)),
		"truncated positive": frameBytes(diag(0x63, 0xF4, []byte{0x62})),
		"malformed IP":       frameBytes(diag(0x63, 0xF4, []byte{0x62, 0x17, 0x2A, 0, 1})),
		"diagnostic frame":   frameBytes(hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{1}}),
		"HSFZ error":         frameBytes(hsfz.Frame{Control: hsfz.ControlErrAppNotReady}),
		"unexpected HSFZ":    frameBytes(hsfz.Frame{Control: 0x1234}),
		"exceeds limit":      oversized,
		"header":             []byte{0, 0},
		"body":               []byte{0, 0, 0, 4, 0, 1, 0x63},
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			info, err := pipeQuery(t, Options{}, func(c net.Conn) error {
				if e := readRequest(c, 0xF4); e != nil {
					return e
				}
				_, e := c.Write(wire)
				return e
			})
			if err == nil || !strings.Contains(err.Error(), name) || info != (Info{}) {
				t.Fatalf("info=%+v err=%v", info, err)
			}
		})
	}
}
func TestAddressValidation(t *testing.T) {
	for _, s := range []string{"0.1.2.3", "0.0.0.0", "127.0.0.1", "224.1.2.3", "239.255.255.250", "255.255.255.255", "240.1.2.3"} {
		t.Run(s, func(t *testing.T) {
			f := positive()
			a := netip.MustParseAddr(s).As4()
			copy(f.Body[6:10], a[:])
			info, err := pipeQuery(t, Options{}, func(c net.Conn) error {
				if e := readRequest(c, 0xF4); e != nil {
					return e
				}
				return hsfz.WriteFrame(c, f)
			})
			if err == nil || info != (Info{}) {
				t.Fatalf("%+v %v", info, err)
			}
		})
	}
}
func TestFrameBudgetAndNoOffsetScan(t *testing.T) {
	for _, payload := range [][]byte{{0x7F, 0x22, 0x78}, append([]byte{0x01}, positive().Body[2:]...)} {
		_, err := pipeQuery(t, Options{MaxFrames: 1}, func(c net.Conn) error {
			if e := readRequest(c, 0xF4); e != nil {
				return e
			}
			return hsfz.WriteFrame(c, diag(0x63, 0xF4, payload))
		})
		if err == nil || !strings.Contains(err.Error(), "frame limit") {
			t.Fatal(err)
		}
	}
}
func TestOptionsAndOpenBudget(t *testing.T) {
	never := openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		t.Fatal("unexpected open")
		return nil, nil
	})
	for _, o := range []Options{{Timeout: -1}, {Timeout: 31 * time.Second}, {MaxFrames: -1}, {MaxFrames: 1025}} {
		if _, e := Discover(context.Background(), never, gateway, o); e == nil {
			t.Fatal(o)
		}
	}
	for _, a := range []string{"[::1]:6801", "0.0.0.0:6801", "224.0.0.1:6801", "127.0.0.1:0"} {
		if _, e := Discover(context.Background(), never, netip.MustParseAddrPort(a), Options{}); e == nil {
			t.Fatal(a)
		}
	}
	opener := openFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	if _, e := Discover(context.Background(), opener, gateway, Options{Timeout: 10 * time.Millisecond}); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal(e)
	}
}
func TestCustomTesterAndRawValues(t *testing.T) {
	f := positive()
	f.Body[1] = 0xF1
	copy(f.Body[10:], []byte{127, 0, 0, 1, 255, 255, 255, 255})
	info, err := pipeQuery(t, Options{Tester: 0xF1}, func(c net.Conn) error {
		if e := readRequest(c, 0xF1); e != nil {
			return e
		}
		return hsfz.WriteFrame(c, f)
	})
	if err != nil || info.Netmask.String() != "127.0.0.1" || info.Third.String() != "255.255.255.255" {
		t.Fatalf("%+v %v", info, err)
	}
}

func TestSessionCancellationIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	started := make(chan struct{})
	clientDone := make(chan error, 1)
	go func() {
		_, e := session.Run(ctx, b, session.Options{Role: session.RoleClient, OnTunnel: func(_ context.Context, h proto.OpenTunnel, st transport.Stream) {
			if h.Target == gateway.String() {
				if e := readRequest(st, 0xF4); e != nil {
					clientDone <- e
					return
				}
				close(started)
				_, e := io.Copy(io.Discard, st)
				clientDone <- e
				return
			}
			_, _ = io.Copy(st, st)
		}})
		clientDone <- e
	}()
	op, e := session.Run(ctx, a, session.Options{Role: session.RoleOperator, Initiator: true, Trusted: func(identity.Fingerprint) bool { return true }})
	if e != nil {
		t.Fatal(e)
	}
	if e := <-clientDone; e != nil {
		t.Fatal(e)
	}
	other, e := op.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: "127.0.0.1:6811"})
	if e != nil {
		t.Fatal(e)
	}
	defer other.Close()
	queryCtx, stop := context.WithCancel(ctx)
	result := make(chan error, 1)
	go func() {
		info, e := Discover(queryCtx, op, gateway, Options{})
		if info != (Info{}) {
			result <- errors.New("nonzero canceled result")
			return
		}
		result <- e
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stop()
	select {
	case e := <-result:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-ctx.Done():
		t.Fatal("query did not unblock")
	}
	// An already-open stream and a newly-opened stream both survive query cancellation.
	for i := 0; i < 2; i++ {
		if i == 1 {
			other, e = op.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: "127.0.0.1:6811"})
			if e != nil {
				t.Fatal(e)
			}
			defer other.Close()
		}
		if _, e := other.Write([]byte("ok")); e != nil {
			t.Fatal(e)
		}
		var buf [2]byte
		if _, e := io.ReadFull(other, buf[:]); e != nil || string(buf[:]) != "ok" {
			t.Fatalf("%q %v", buf, e)
		}
	}
	if op.Err() != nil {
		t.Fatal(op.Err())
	}
}

func TestBlockedIOBudget(t *testing.T) {
	for _, stage := range []string{"request", "response", "alive"} {
		t.Run(stage, func(t *testing.T) {
			a, b := net.Pipe()
			done := make(chan struct{})
			go func() {
				defer close(done)
				defer b.Close()
				if stage == "request" {
					var one [1]byte
					_, _ = b.Read(one[:])
					<-time.After(40 * time.Millisecond)
					return
				}
				if e := readRequest(b, 0xF4); e != nil {
					return
				}
				if stage == "alive" {
					_ = hsfz.WriteFrame(b, hsfz.Frame{Control: hsfz.ControlAliveCheck})
				}
				// The peer never sends a query response or consumes an alive reply.
				var one [1]byte
				if stage == "alive" {
					_, _ = b.Read(one[:])
				}
				<-time.After(40 * time.Millisecond)
			}()
			opener := openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return pipeStream{a}, nil })
			info, err := Discover(context.Background(), opener, gateway, Options{Timeout: 10 * time.Millisecond})
			if !errors.Is(err, context.DeadlineExceeded) || info != (Info{}) {
				t.Fatalf("%+v %v", info, err)
			}
			<-done
		})
	}
}

func TestIgnoredACKBodies(t *testing.T) {
	for _, body := range [][]byte{nil, {0xF4}, {0xF4, 0x63, 0x22, 0x17, 0x2A}} {
		t.Run(fmt.Sprintf("length-%d", len(body)), func(t *testing.T) {
			info, err := pipeQuery(t, Options{}, func(c net.Conn) error {
				if err := readRequest(c, 0xF4); err != nil {
					return err
				}
				if err := hsfz.WriteFrame(c, hsfz.Frame{Control: hsfz.ControlAck, Body: body}); err != nil {
					return err
				}
				return hsfz.WriteFrame(c, positive())
			})
			if err != nil || info.Address.String() != "169.254.42.7" {
				t.Fatalf("%+v %v", info, err)
			}
		})
	}
}

func TestACKFloodFrameBudget(t *testing.T) {
	info, err := pipeQuery(t, Options{MaxFrames: 3}, func(c net.Conn) error {
		if err := readRequest(c, 0xF4); err != nil {
			return err
		}
		for _, body := range [][]byte{nil, {0xF4}, {0xF4, 0x63, 0x22, 0x17, 0x2A}} {
			if err := hsfz.WriteFrame(c, hsfz.Frame{Control: hsfz.ControlAck, Body: body}); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "frame limit exceeded") || info != (Info{}) {
		t.Fatalf("%+v %v", info, err)
	}
}
