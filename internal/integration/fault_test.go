package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
)

const streamFaultBudget = 5 * time.Second

// These are stream-level delays and pauses below inner TLS, not packet loss.
// Every successful Read/Write forwards the original bytes exactly once.
func TestStreamFaultsThroughHub(t *testing.T) {
	for _, embedded := range []bool{false, true} {
		t.Run(fmt.Sprintf("embedded=%t", embedded), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
			defer cancel()
			wrapped := make(chan *streamFaultConn, 1)
			ct, ot, token := connectPair(t, ctx, embedded, true, func(conn net.Conn) net.Conn {
				fault := newStreamFaultConn(conn)
				wrapped <- fault
				return fault
			})
			var fault *streamFaultConn
			select {
			case fault = <-wrapped:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			var workers sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				_ = fault.Close()
				_ = ct.Close(nil)
				_ = ot.Close(nil)
				done := make(chan struct{})
				go func() { workers.Wait(); close(done) }()
				streamFaultWait(t, done, "test I/O workers")
				streamFaultWait(t, fault.drained, "injected connection I/O")
			})
			var tunnelCalls atomic.Int64
			headers := make(chan proto.OpenTunnel, 4)
			echoDone := make(chan error, 4)
			const echoTarget = "stream-fault-echo"
			c, o := runSessions(t, ctx, ct, ot,
				session.Options{Role: session.RoleClient, Initiator: true, Token: token, AllowLinkTest: true, OnTunnel: func(_ context.Context, h proto.OpenTunnel, st transport.Stream) {
					tunnelCalls.Add(1)
					headers <- h
					if h.Kind != "tcp" || h.Target != echoTarget {
						echoDone <- fmt.Errorf("unexpected echo header: %+v", h)
						return
					}
					// No gateway dial: the only normal tunnel is a transparent echo.
					_, err := io.Copy(st, st)
					if err == nil {
						err = st.CloseWrite()
					}
					echoDone <- err
				}},
				session.Options{Role: session.RoleOperator, AllowLinkTest: true, AcceptToken: func(peer identity.Fingerprint, got [16]byte) bool {
					return peer == ot.Peer() && got == token
				}})
			for _, s := range []*session.Session{c, o} {
				if len(s.NegotiatedCapabilities) != 1 || s.NegotiatedCapabilities[0] != linktest.Capability {
					t.Fatal(s.NegotiatedCapabilities)
				}
			}
			fault.read.setDelay(3 * time.Millisecond)
			fault.write.setDelay(3 * time.Millisecond)
			options := linktest.Options{Samples: 3, PayloadBytes: 64 << 10, FrameBytes: 4 << 10, Timeout: 5 * time.Second}
			result, err := o.TestLink(ctx, options)
			if err != nil {
				t.Fatal(err)
			}
			if result.Samples != options.Samples || result.BytesSent != uint64(options.PayloadBytes) || result.BytesReceived != uint64(options.PayloadBytes) || result.EchoedBytes != uint64(options.PayloadBytes-options.Samples*32) || result.RTTP50 <= 0 || result.RTTP95 < result.RTTP50 || result.EchoBytesPerSecond <= 0 {
				t.Fatalf("delayed link test: %+v", result)
			}
			if tunnelCalls.Load() != 0 {
				t.Fatal("link test reached vehicle OnTunnel")
			}
			if fault.read.delayed.Load() == 0 || fault.write.delayed.Load() == 0 {
				t.Fatal("delay injection was not exercised in both directions")
			}
			t.Logf("stream Read/Write delay=3ms; measured echo RTT p50=%s p95=%s, payload=%d each direction", result.RTTP50, result.RTTP95, result.BytesSent)

			ordinary, err := o.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: echoTarget, Origin: "stream-fault-test"})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = ordinary.Close() })
			select {
			case header := <-headers:
				if header.Kind != "tcp" || header.Target != echoTarget {
					t.Fatal(header)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			streamFaultEcho(t, ctx, ordinary, 0)
			for _, direction := range []string{"write", "read"} {
				gate := &fault.write
				if direction == "read" {
					gate = &fault.read
				}
				pause := gate.pause()
				testCtx, stopTest := context.WithTimeout(ctx, time.Second)
				type outcome struct {
					result linktest.Result
					err    error
				}
				done := make(chan outcome, 1)
				workers.Go(func() { r, err := c.TestLink(testCtx, options); done <- outcome{r, err} })
				streamFaultWait(t, pause.entered, direction+" pause entered")
				if direction == "write" {
					stopTest()
				}
				// Deadline is a synchronization event, not a wall-clock assertion.
				// FIN may still be waiting on the paused underlying writer.
				streamFaultWait(t, testCtx.Done(), direction+" test cancellation")
				gate.resume()
				select {
				case got := <-done:
					canceled, timedOut := errors.Is(got.err, context.Canceled), errors.Is(got.err, context.DeadlineExceeded)
					if (!canceled && !timedOut) || got.result.TimedOut != timedOut || got.result.Interrupted != canceled {
						t.Fatalf("%s pause result=%+v error=%v", direction, got.result, got.err)
					}
				case <-time.After(streamFaultBudget):
					t.Fatalf("%s test did not return after pause release", direction)
				}
				stopTest()
				streamFaultAlive(t, c, o, ct, ot)
				sequence := byte(1)
				if direction == "read" {
					sequence = 2
				}
				streamFaultEcho(t, ctx, ordinary, sequence)
				if tunnelCalls.Load() != 1 {
					t.Fatal("canceled link test reached OnTunnel")
				}
			}

			// Close must release both injected write and read-delivery waits,
			// then propagate through Hub to the other peer.
			readPause, writePause := fault.read.pause(), fault.write.pause()
			clientSend, operatorSend := make(chan error, 1), make(chan error, 1)
			workers.Go(func() { clientSend <- c.Send(proto.Log{Msg: "blocked client write"}) })
			workers.Go(func() { operatorSend <- o.Send(proto.Log{Msg: "blocked client read"}) })
			type readResult struct {
				n   int
				err error
			}
			readDone := make(chan readResult, 1)
			workers.Go(func() { var b [1]byte; n, err := ordinary.Read(b[:]); readDone <- readResult{n, err} })
			streamFaultWait(t, readPause.entered, "final read pause")
			streamFaultWait(t, writePause.entered, "final write pause")
			_ = fault.Close()
			for _, s := range []*session.Session{c, o} {
				streamFaultWait(t, s.Done(), "session disconnect")
				if s.Err() == nil {
					t.Fatal("session lost terminal error")
				}
			}
			for _, ts := range []transport.Session{ct, ot} {
				streamFaultWait(t, ts.Done(), "transport disconnect")
			}
			streamFaultWait(t, fault.drained, "closed connection I/O")
			for _, done := range []<-chan error{clientSend, operatorSend, echoDone} {
				select {
				case <-done:
				case <-time.After(streamFaultBudget):
					t.Fatal("session I/O survived physical disconnect")
				}
			}
			select {
			case got := <-readDone:
				if got.n != 0 || got.err == nil {
					t.Fatalf("ordinary stream replay or missing disconnect: bytes=%d error=%v", got.n, got.err)
				}
			case <-time.After(streamFaultBudget):
				t.Fatal("ordinary stream read survived disconnect")
			}
			if tunnelCalls.Load() != 1 {
				t.Fatal("unexpected vehicle stream")
			}
			// connectPair cleanup cancels Serve and bounds the join of its real
			// Hub handlers, after the disconnect assertions above have completed.
		})
	}
}

func streamFaultWait(t *testing.T, done <-chan struct{}, description string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(streamFaultBudget):
		t.Fatalf("timed out waiting for %s", description)
	}
}

func streamFaultAlive(t *testing.T, c, o *session.Session, ct, ot transport.Session) {
	t.Helper()
	for _, done := range []<-chan struct{}{c.Done(), o.Done(), ct.Done(), ot.Done()} {
		select {
		case <-done:
			t.Fatal("individual cancellation closed the shared session")
		default:
		}
	}
}

func streamFaultEcho(t *testing.T, ctx context.Context, st transport.Stream, sequence byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, streamFaultBudget)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = st.Close() })
	defer stop()
	want := bytes.Repeat([]byte{sequence, 0xff, 0, 0x42}, 4096)
	if _, err := st.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ordinary stream reordered or replayed sequence %d", sequence)
	}
}

type streamFaultPause struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type streamFaultGate struct {
	mu      sync.Mutex
	delay   time.Duration
	hold    *streamFaultPause
	delayed atomic.Uint64
}

func (g *streamFaultGate) setDelay(delay time.Duration) { g.mu.Lock(); g.delay = delay; g.mu.Unlock() }

func (g *streamFaultGate) pause() *streamFaultPause {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hold != nil {
		panic("stream fault direction already paused")
	}
	g.hold = &streamFaultPause{entered: make(chan struct{}), release: make(chan struct{})}
	return g.hold
}

func (g *streamFaultGate) resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.hold != nil {
		close(g.hold.release)
		g.hold = nil
	}
}

func (g *streamFaultGate) wait(closed <-chan struct{}) error {
	g.mu.Lock()
	hold := g.hold
	g.mu.Unlock()
	if hold != nil {
		hold.once.Do(func() { close(hold.entered) })
		select {
		case <-hold.release:
		case <-closed:
			return net.ErrClosed
		}
	}
	select {
	case <-closed:
		return net.ErrClosed
	default:
		return nil
	}
}

func (g *streamFaultGate) delayIO(closed <-chan struct{}) error {
	g.mu.Lock()
	delay := g.delay
	g.mu.Unlock()
	if delay > 0 {
		g.delayed.Add(1)
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-closed:
			return net.ErrClosed
		}
	}
	return g.wait(closed)
}

type streamFaultConn struct {
	net.Conn
	read, write streamFaultGate
	closed      chan struct{}
	drained     chan struct{}
	once        sync.Once
	closeErr    error
	mu          sync.Mutex
	closing     bool
	active      int
}

func newStreamFaultConn(conn net.Conn) *streamFaultConn {
	return &streamFaultConn{Conn: conn, closed: make(chan struct{}), drained: make(chan struct{})}
}

func (c *streamFaultConn) begin() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing {
		return false
	}
	c.active++
	return true
}

func (c *streamFaultConn) end() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.active--
	if c.closing && c.active == 0 {
		close(c.drained)
	}
}

func (c *streamFaultConn) Read(p []byte) (int, error) {
	if !c.begin() {
		return 0, net.ErrClosed
	}
	defer c.end()
	if err := c.read.wait(c.closed); err != nil {
		return 0, err
	}
	n, err := c.Conn.Read(p)
	if n > 0 {
		// Recheck after Read so arming a pause also catches an in-flight read.
		if waitErr := c.read.delayIO(c.closed); waitErr != nil {
			return 0, waitErr
		}
	}
	return n, err
}

func (c *streamFaultConn) Write(p []byte) (int, error) {
	if !c.begin() {
		return 0, net.ErrClosed
	}
	defer c.end()
	if err := c.write.delayIO(c.closed); err != nil {
		return 0, err
	}
	return c.Conn.Write(p)
}

func (c *streamFaultConn) Close() error {
	c.once.Do(func() {
		c.mu.Lock()
		c.closing = true
		close(c.closed)
		if c.active == 0 {
			close(c.drained)
		}
		c.mu.Unlock()
		// The wrapped connection is outer TLS. Expire its I/O before closing
		// so close_notify cannot delay the injected physical disconnect.
		_ = c.Conn.SetDeadline(time.Now())
		c.closeErr = c.Conn.Close()
	})
	return c.closeErr
}
