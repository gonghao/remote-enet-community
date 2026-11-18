package tunnel

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/testutil/memtransport"
)

func tcpPair(t *testing.T) (*net.TCPConn, *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ch := make(chan *net.TCPConn, 1)
	errs := make(chan error, 1)
	go func() {
		c, err := ln.AcceptTCP()
		if err != nil {
			errs <- err
			return
		}
		ch <- c
	}()
	a, err := net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	var b *net.TCPConn
	select {
	case b = <-ch:
	case err := <-errs:
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	_ = a.SetDeadline(time.Now().Add(3 * time.Second))
	_ = b.SetDeadline(time.Now().Add(3 * time.Second))
	return a, b
}
func TestHalfCloseBothDirections(t *testing.T) {
	for _, first := range []string{"stream", "tcp"} {
		t.Run(first, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
			remote, err := a.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			local, err := b.AcceptStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conn, peer := tcpPair(t)
			counter := &testCounter{}
			tap := &testTap{}
			stats := Stats{Counter: counter, Payload: tap}
			done := make(chan error, 1)
			go func() { done <- Pipe(ctx, local, conn, &stats) }()
			if first == "stream" {
				_, _ = remote.Write([]byte("request"))
				_ = remote.CloseWrite()
				req, err := io.ReadAll(peer)
				if err != nil || string(req) != "request" {
					t.Fatalf("%q %v", req, err)
				}
				select {
				case err := <-done:
					t.Fatalf("premature Pipe exit: %v", err)
				default:
				}
				_, _ = peer.Write([]byte("reply"))
				_ = peer.CloseWrite()
				res, err := io.ReadAll(remote)
				if err != nil || string(res) != "reply" {
					t.Fatalf("%q %v", res, err)
				}
			} else {
				_, _ = peer.Write([]byte("reply"))
				_ = peer.CloseWrite()
				res, err := io.ReadAll(remote)
				if err != nil || string(res) != "reply" {
					t.Fatalf("%q %v", res, err)
				}
				select {
				case err := <-done:
					t.Fatalf("premature Pipe exit: %v", err)
				default:
				}
				_, _ = remote.Write([]byte("request"))
				_ = remote.CloseWrite()
				req, err := io.ReadAll(peer)
				if err != nil || string(req) != "request" {
					t.Fatalf("%q %v", req, err)
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if string(tap.in) != "request" || string(tap.out) != "reply" {
				t.Fatalf("payload: %q %q", tap.in, tap.out)
			}
			if counter.in.Load() != 7 || counter.out.Load() != 5 {
				t.Fatalf("incremental counter: in=%d out=%d", counter.in.Load(), counter.out.Load())
			}
			if stats.BytesIn.Load() != 7 || stats.BytesOut.Load() != 5 || stats.StartedAt.IsZero() {
				t.Fatalf("stats: %d %d", stats.BytesIn.Load(), stats.BytesOut.Load())
			}
		})
	}
}
func TestCancelAndTransportFailure(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "transport"}[failure], func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
			_, err := a.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			local, err := b.AcceptStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			conn, _ := tcpPair(t)
			done := make(chan error, 1)
			go func() { done <- Pipe(ctx, local, conn, nil) }()
			if failure {
				_ = a.Close(errors.New("lost"))
				cancel()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("Pipe leaked")
			}
		})
	}
}
func TestUnsupportedHalfClose(t *testing.T) {
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	ctx := context.Background()
	_, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	st, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	x, y := net.Pipe()
	defer y.Close()
	if err := Pipe(ctx, st, x, nil); err == nil {
		t.Fatal("unsupported connection accepted")
	}
}

type cancelConn struct{ net.Conn }

func (c cancelConn) CloseWrite() error { return c.Conn.Close() }
func TestCancelBlockedWritesBothDirections(t *testing.T) {
	for _, fromA := range []bool{true, false} {
		t.Run(map[bool]string{true: "a-to-b", false: "b-to-a"}[fromA], func(t *testing.T) {
			a, remoteA := net.Pipe()
			defer remoteA.Close()
			b, remoteB := net.Pipe()
			defer remoteB.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			stats := &Stats{Counter: &testCounter{}, Payload: &testTap{}}
			go func() { result <- Pipe(ctx, cancelConn{a}, cancelConn{b}, stats) }()
			src := remoteA
			if !fromA {
				src = remoteB
			}
			copied := make(chan error, 1)
			go func() { _, err := src.Write(make([]byte, 1<<17)); copied <- err }()
			// A source write exceeding the copy buffer blocks without a destination reader.
			time.Sleep(10 * time.Millisecond)
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("copy workers leaked")
			}
			select {
			case <-copied:
			case <-time.After(time.Second):
				t.Fatal("source writer leaked")
			}
		})
	}
}

type testCounter struct{ in, out atomic.Uint64 }

func (c *testCounter) AddIn(n uint64)  { c.in.Add(n) }
func (c *testCounter) AddOut(n uint64) { c.out.Add(n) }

type shortWriter struct{}

func (shortWriter) Write([]byte) (int, error) { return 2, io.ErrShortWrite }
func TestCounterCountsSuccessfulPartialWrites(t *testing.T) {
	var count atomic.Uint64
	counter := &testCounter{}
	tap := &testTap{}
	w := countingWriter{Writer: shortWriter{}, count: &count, add: counter.AddIn, payload: tap.In}
	n, err := w.Write([]byte("four"))
	if string(tap.in) != "fo" {
		t.Fatalf("partial payload: %q", tap.in)
	}
	if n != 2 || !errors.Is(err, io.ErrShortWrite) || count.Load() != 2 || counter.in.Load() != 2 {
		t.Fatalf("partial write counted attempted bytes: n=%d count=%d counter=%d err=%v", n, count.Load(), counter.in.Load(), err)
	}
}

type testTap struct {
	mu      sync.Mutex
	in, out []byte
}

func (t *testTap) In(p []byte)  { t.mu.Lock(); defer t.mu.Unlock(); t.in = append(t.in, p...) }
func (t *testTap) Out(p []byte) { t.mu.Lock(); defer t.mu.Unlock(); t.out = append(t.out, p...) }

// ReaderFrom must remain hidden by the destination wrapper, including when the
// source offers WriterTo instead of the generic copy loop.
type optimizedWriter struct {
	bytes.Buffer
	reads int
}

func (w *optimizedWriter) ReadFrom(r io.Reader) (int64, error) {
	w.reads++
	return w.Buffer.ReadFrom(r)
}
func TestTapCannotBeBypassedByCopyOptimization(t *testing.T) {
	for _, writerTo := range []bool{false, true} {
		dst := &optimizedWriter{}
		tap := &testTap{}
		var src io.Reader = bytes.NewBufferString("opaque")
		if !writerTo {
			src = struct{ io.Reader }{src}
		}
		n, err := io.Copy(countingWriter{Writer: dst, payload: tap.In}, src)
		if err != nil || n != 6 || dst.String() != "opaque" || string(tap.in) != "opaque" || dst.reads != 0 {
			t.Fatalf("copy: n=%d err=%v dst=%q tap=%q reads=%d", n, err, dst.String(), tap.in, dst.reads)
		}
	}
}

type partialConn struct{ net.Conn }

func (c partialConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p[:2])
	if err != nil {
		return n, err
	}
	return n, io.ErrShortWrite
}
func (c partialConn) CloseWrite() error { return c.Conn.Close() }

func TestPipeTapRecordsPartialWriteBeforeFailure(t *testing.T) {
	for _, fromA := range []bool{true, false} {
		t.Run(map[bool]string{true: "in", false: "out"}[fromA], func(t *testing.T) {
			a, remoteA := net.Pipe()
			b, remoteB := net.Pipe()
			defer remoteA.Close()
			defer remoteB.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			tap := &testTap{}
			counter := &testCounter{}
			stats := &Stats{Counter: counter, Payload: tap}
			done := make(chan error, 1)
			go func() { done <- Pipe(ctx, partialConn{a}, partialConn{b}, stats) }()
			src, dst := remoteA, remoteB
			if !fromA {
				src, dst = remoteB, remoteA
			}
			sent := make(chan struct{})
			go func() { defer close(sent); _, _ = src.Write([]byte("four")) }()
			data, err := io.ReadAll(dst)
			if err != nil || string(data) != "fo" {
				t.Fatalf("destination: %q %v", data, err)
			}
			if err := <-done; !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("pipe: %v", err)
			}
			<-sent
			got, count, increment := tap.in, stats.BytesIn.Load(), counter.in.Load()
			other := tap.out
			if !fromA {
				got, count, increment, other = tap.out, stats.BytesOut.Load(), counter.out.Load(), tap.in
			}
			if string(got) != "fo" || len(other) != 0 || count != 2 || increment != 2 {
				t.Fatalf("payload=%q other=%q count=%d increment=%d", got, other, count, increment)
			}
		})
	}
}
