package linktest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func pipe(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func await(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("stream worker did not stop")
		return nil
	}
}

func requestPayload(samples uint32, total uint64, frameBytes, millis uint32) []byte {
	p := make([]byte, 20)
	binary.BigEndian.PutUint32(p, samples)
	binary.BigEndian.PutUint64(p[4:], total)
	binary.BigEndian.PutUint32(p[12:], frameBytes)
	binary.BigEndian.PutUint32(p[16:], millis)
	return p
}

func TestEchoMeasurements(t *testing.T) {
	for _, options := range []Options{
		{},
		{Samples: 3, PayloadBytes: 96},
		{Samples: MaxSamples, PayloadBytes: MaxPayloadBytes, FrameBytes: maxDataBytes, Timeout: MaxDuration},
	} {
		a, b := pipe(t)
		done := make(chan error, 1)
		go func() { done <- Serve(t.Context(), b) }()
		result, err := Run(t.Context(), a, options)
		if err != nil {
			t.Fatal(err)
		}
		if err := await(t, done); err != nil {
			t.Fatal(err)
		}
		o, err := options.Normalize()
		if err != nil {
			t.Fatal(err)
		}
		if result.Samples != o.Samples || result.RequestedSamples != o.Samples || result.BytesSent != uint64(o.PayloadBytes) || result.BytesReceived != uint64(o.PayloadBytes) || result.EchoedBytes != uint64(o.PayloadBytes-o.Samples*probeBytes) || result.RTTP50 <= 0 || result.RTTP95 < result.RTTP50 || result.Duration <= 0 || result.Interrupted || result.TimedOut {
			t.Fatalf("%+v", result)
		}
		if result.EchoedBytes > 0 && (result.EchoBytesPerSecond <= 0 || result.PayloadDuration <= 0 || result.PayloadDuration > result.Duration) {
			t.Fatal(result)
		}
		if result.EchoedBytes == 0 && (result.PayloadDuration != 0 || result.EchoBytesPerSecond != 0) {
			t.Fatal(result)
		}
	}
}

func TestOptionsLimits(t *testing.T) {
	for _, o := range []Options{
		{Samples: -1}, {Samples: MaxSamples + 1}, {PayloadBytes: -1},
		{PayloadBytes: MaxPayloadBytes + 1}, {Samples: 2, PayloadBytes: probeBytes},
		{FrameBytes: probeBytes - 1}, {FrameBytes: maxDataBytes + 1},
		{Timeout: -1}, {Timeout: time.Microsecond}, {Timeout: MaxDuration + 1},
	} {
		if _, err := o.Normalize(); !errors.Is(err, ErrOptions) {
			t.Fatalf("%+v: %v", o, err)
		}
	}
	if _, err := (Options{Samples: 1, PayloadBytes: probeBytes, FrameBytes: probeBytes, Timeout: time.Millisecond}).Normalize(); err != nil {
		t.Fatal(err)
	}
}

func TestMalformedRequestsAndFrames(t *testing.T) {
	for name, input := range map[string][]byte{
		"short-frame":          {0, 0, 0, 4},
		"oversize-frame":       {0, 1, 0, 1},
		"empty-request":        encodedFrame(request, 0, nil),
		"wrong-type":           encodedFrame(data, 0, nil),
		"wrong-sequence":       encodedFrame(request, 1, requestPayload(1, 32, 32, 100)),
		"zero-samples":         encodedFrame(request, 0, requestPayload(0, 32, 32, 100)),
		"excess-samples":       encodedFrame(request, 0, requestPayload(MaxSamples+1, MaxPayloadBytes, 32, 100)),
		"excess-payload":       encodedFrame(request, 0, requestPayload(1, MaxPayloadBytes+1, 32, 100)),
		"uint64-payload":       encodedFrame(request, 0, requestPayload(1, ^uint64(0), 32, 100)),
		"insufficient-payload": encodedFrame(request, 0, requestPayload(2, 32, 32, 100)),
		"excess-frame":         encodedFrame(request, 0, requestPayload(1, 32, MaxFrameBytes, 100)),
		"small-frame":          encodedFrame(request, 0, requestPayload(1, 32, 1, 100)),
		"zero-time":            encodedFrame(request, 0, requestPayload(1, 32, 32, 0)),
		"excess-time":          encodedFrame(request, 0, requestPayload(1, 32, 32, 15001)),
	} {
		t.Run(name, func(t *testing.T) {
			a, b := pipe(t)
			done := make(chan error, 1)
			go func() { done <- Serve(t.Context(), b) }()
			if _, err := a.Write(input); err != nil {
				t.Fatal(err)
			}
			if err := await(t, done); !errors.Is(err, ErrProtocol) || errors.Is(err, ErrTransport) {
				t.Fatal(err)
			}
		})
	}
}

func encodedFrame(kind byte, seq uint32, payload []byte) []byte {
	var b bytes.Buffer
	_, _ = writeFrame(&b, kind, seq, payload)
	return b.Bytes()
}

func TestServerOwnBudgetAndSlowReader(t *testing.T) {
	for _, slowReader := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle-client", true: "blocked-echo"}[slowReader], func(t *testing.T) {
			a, b := pipe(t)
			done := make(chan error, 1)
			go func() { done <- Serve(t.Context(), b) }()
			if _, err := writeFrame(a, request, 0, requestPayload(1, probeBytes, 32, 50)); err != nil {
				t.Fatal(err)
			}
			buf := make([]byte, MaxFrameBytes)
			if _, err := expect(a, buf, ready, 0, nil, nil); err != nil {
				t.Fatal(err)
			}
			if slowReader {
				if _, err := writeFrame(a, probe, 1, make([]byte, probeBytes)); err != nil {
					t.Fatal(err)
				}
			}
			if err := await(t, done); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

func TestServerHardBudgetBeforeRequest(t *testing.T) {
	t.Parallel()
	a, b := pipe(t)
	started := time.Now()
	err := Serve(t.Context(), b)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < MaxDuration || elapsed > MaxDuration+2*time.Second {
		t.Fatal(elapsed)
	}
	var p [1]byte
	if _, err := a.Read(p[:]); err == nil {
		t.Fatal("server did not close stream")
	}
}

func TestBlackholeCancellationAndCleanup(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "timeout"}[timeout], func(t *testing.T) {
			a, b := pipe(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			requestRead := make(chan struct{})
			peerDone := make(chan error, 1)
			go func() {
				_, _, err := readFrame(b, make([]byte, MaxFrameBytes))
				close(requestRead)
				var p [1]byte
				if err == nil {
					_, err = b.Read(p[:])
				}
				peerDone <- err
			}()
			type outcome struct {
				result Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() { r, err := Run(ctx, a, Options{Timeout: 50 * time.Millisecond}); done <- outcome{r, err} }()
			<-requestRead
			if !timeout {
				cancel()
			}
			select {
			case got := <-done:
				want := context.Canceled
				if timeout {
					want = context.DeadlineExceeded
				}
				if !errors.Is(got.err, want) || got.result.TimedOut != timeout || got.result.Interrupted == timeout || got.result.Samples != 0 {
					t.Fatalf("%+v %v", got.result, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("client leaked")
			}
			if err := await(t, peerDone); err == nil {
				t.Fatal("client did not close stream")
			}
		})
	}
}

func TestPartialResultAndFailureClassification(t *testing.T) {
	for _, malformed := range []bool{false, true} {
		t.Run(map[bool]string{false: "transport", true: "protocol"}[malformed], func(t *testing.T) {
			a, b := pipe(t)
			done := make(chan error, 1)
			go func() {
				defer b.Close()
				buf := make([]byte, MaxFrameBytes)
				if _, _, err := readFrame(b, buf); err != nil {
					done <- err
					return
				}
				if _, err := writeFrame(b, ready, 0, nil); err != nil {
					done <- err
					return
				}
				for seq := uint32(1); seq <= 2; seq++ {
					f, _, err := readFrame(b, buf)
					if err != nil {
						done <- err
						return
					}
					if seq == 2 {
						if malformed {
							_, err = writeFrame(b, probeEcho, seq+1, f.payload)
						}
						done <- err
						return
					}
					if _, err := writeFrame(b, probeEcho, seq, f.payload); err != nil {
						done <- err
						return
					}
				}
			}()
			r, err := Run(t.Context(), a, Options{Samples: 3})
			want := ErrTransport
			if malformed {
				want = ErrProtocol
			}
			if !errors.Is(err, want) || r.Samples != 1 || r.BytesSent != 64 || r.RTTP50 <= 0 || !r.Interrupted {
				t.Fatalf("%+v %v", r, err)
			}
			if err := await(t, done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type delayedConn struct {
	net.Conn
	delay  time.Duration
	closed chan struct{}
	once   sync.Once
}

func (c *delayedConn) Read(p []byte) (int, error) {
	timer := time.NewTimer(c.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return c.Conn.Read(p)
	case <-c.closed:
		return 0, net.ErrClosed
	}
}
func (c *delayedConn) Close() error { c.once.Do(func() { close(c.closed) }); return c.Conn.Close() }

func TestDelayedEchoRTT(t *testing.T) {
	a, b := pipe(t)
	done := make(chan error, 1)
	go func() {
		done <- Serve(t.Context(), &delayedConn{Conn: b, delay: 2 * time.Millisecond, closed: make(chan struct{})})
	}()
	r, err := Run(t.Context(), a, Options{Samples: 3, PayloadBytes: 96})
	if err != nil {
		t.Fatal(err)
	}
	if r.RTTP50 < 2*time.Millisecond || r.RTTP95 < r.RTTP50 {
		t.Fatal(r)
	}
	if err := await(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestUnexpectedProbeAndExtraPayload(t *testing.T) {
	for _, packet := range [][]byte{
		encodedFrame(probe, 2, make([]byte, 32)),
		encodedFrame(probe, 1, make([]byte, 33)),
		encodedFrame(data, 1, make([]byte, 32)),
	} {
		a, b := pipe(t)
		done := make(chan error, 1)
		go func() { done <- Serve(t.Context(), b) }()
		if _, err := writeFrame(a, request, 0, requestPayload(1, 32, 32, 1000)); err != nil {
			t.Fatal(err)
		}
		if _, err := expect(a, make([]byte, MaxFrameBytes), ready, 0, nil, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := a.Write(packet); err != nil {
			t.Fatal(err)
		}
		if err := await(t, done); !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	}
}

func TestPartialFrameAndZeroWrite(t *testing.T) {
	a, b := pipe(t)
	done := make(chan error, 1)
	go func() { done <- Serve(t.Context(), b) }()
	_, _ = a.Write([]byte{0, 0})
	_ = a.Close()
	if err := await(t, done); !errors.Is(err, ErrTransport) || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	if _, err := writeFrame(zeroWriter{}, probe, 1, []byte{1}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
}

type zeroWriter struct{}

func (zeroWriter) Write([]byte) (int, error) { return 0, nil }
