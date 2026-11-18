// Package linktest measures a bounded echo exchange between renet peers.
package linktest

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"
)

const (
	// Capability is also the reserved OpenTunnel kind.
	Capability      = "linktest/1"
	MaxPayloadBytes = 8 << 20
	MaxSamples      = 1024
	// MaxFrameBytes includes the length prefix, type and sequence.
	MaxFrameBytes     = 64 << 10
	MaxDuration       = 15 * time.Second
	probeBytes        = 32
	frameHeaderBytes  = 5
	lengthPrefixBytes = 4
	maxDataBytes      = MaxFrameBytes - lengthPrefixBytes - frameHeaderBytes
)

// Protocol errors describe decoded violations; transport errors preserve the
// original I/O cause. Cancellation and deadlines retain context errors.
var (
	ErrUnsupported = errors.New("linktest: capability not negotiated")
	ErrBusy        = errors.New("linktest: test already running")
	ErrOptions     = errors.New("linktest: invalid options")
	ErrProtocol    = errors.New("linktest: invalid protocol")
	ErrTransport   = errors.New("linktest: transport failure")
)

// Options bounds each direction's payload, including the fixed-size RTT probes.
// Zero fields select defaults; negative or excessive values are rejected.
type Options struct {
	Samples      int
	PayloadBytes int
	// FrameBytes is data per bulk frame, excluding type, sequence and length.
	FrameBytes int
	Timeout    time.Duration
}

// Normalize applies defaults and checks hard limits before opening a stream.
func (o Options) Normalize() (Options, error) {
	if o.Samples == 0 {
		o.Samples = 10
	}
	if o.PayloadBytes == 0 {
		o.PayloadBytes = 1 << 20
	}
	if o.FrameBytes == 0 {
		o.FrameBytes = 32 << 10
	}
	if o.Timeout == 0 {
		o.Timeout = 10 * time.Second
	}
	if o.Samples < 1 || o.Samples > MaxSamples || o.PayloadBytes < o.Samples*probeBytes || o.PayloadBytes > MaxPayloadBytes || o.FrameBytes < probeBytes || o.FrameBytes > maxDataBytes || o.Timeout < time.Millisecond || o.Timeout > MaxDuration {
		return Options{}, fmt.Errorf("%w: samples=%d payload=%d frame=%d timeout=%s", ErrOptions, o.Samples, o.PayloadBytes, o.FrameBytes, o.Timeout)
	}
	return o, nil
}

// Result counts application payload only. Samples is the number of validated
// RTT echoes; zero Samples means percentiles were not measured.
type Result struct {
	RequestedSamples int
	Samples          int
	RTTP50           time.Duration
	RTTP95           time.Duration
	BytesSent        uint64
	BytesReceived    uint64
	// EchoedBytes counts validated bulk echoes, excluding the RTT probes.
	EchoedBytes     uint64
	Duration        time.Duration
	PayloadDuration time.Duration
	// EchoBytesPerSecond is bulk payload / stop-and-wait echo time, not a
	// directional capacity estimate or the sum of upload and download rates.
	EchoBytesPerSecond float64
	TimedOut           bool
	Interrupted        bool
}

const (
	request byte = iota + 1
	ready
	probe
	probeEcho
	data
	dataEcho
	finish
	finished
)

// Run owns stream, including on invalid options. Close must unblock Read/Write.
// A failed exchange can return both partial measurements and an error.
func Run(ctx context.Context, stream io.ReadWriteCloser, o Options) (result Result, err error) {
	started := time.Now()
	defer func() {
		result.Duration = time.Since(started)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		result.Interrupted = err != nil && !result.TimedOut
	}()
	o, err = o.Normalize()
	if err != nil {
		_ = stream.Close()
		return result, err
	}
	result.RequestedSamples = o.Samples
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	defer guard(ctx, stream)()
	err = run(ctx, stream, o, &result)
	return result, err
}

func run(ctx context.Context, stream io.ReadWriter, o Options, result *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint32(req, uint32(o.Samples))
	binary.BigEndian.PutUint64(req[4:], uint64(o.PayloadBytes))
	binary.BigEndian.PutUint32(req[12:], uint32(o.FrameBytes))
	// Round up so the server never receives a zero duration for valid input.
	binary.BigEndian.PutUint32(req[16:], uint32((o.Timeout+time.Millisecond-1)/time.Millisecond))
	if _, err := writeFrame(stream, request, 0, req); err != nil {
		return failure(ctx, err)
	}
	buf := make([]byte, MaxFrameBytes)
	if _, err := expect(stream, buf, ready, 0, nil, nil); err != nil {
		return failure(ctx, err)
	}
	rtts := make([]time.Duration, 0, o.Samples)
	defer func() {
		result.Samples = len(rtts)
		if len(rtts) != 0 {
			slices.Sort(rtts)
			result.RTTP50 = rtts[(len(rtts)-1)/2]
			result.RTTP95 = rtts[(95*len(rtts)+99)/100-1]
		}
	}()
	payload := make([]byte, max(probeBytes, o.FrameBytes))
	seq := uint32(1)
	for range o.Samples {
		fill(payload[:probeBytes], seq)
		start := time.Now()
		if err := exchange(ctx, stream, buf, probe, probeEcho, seq, payload[:probeBytes], result); err != nil {
			return err
		}
		rtts = append(rtts, time.Since(start))
		seq++
	}
	remaining := o.PayloadBytes - o.Samples*probeBytes
	if remaining > 0 {
		var err error
		seq, err = runBulk(ctx, stream, buf, payload, seq, remaining, result)
		if err != nil {
			return err
		}
	}
	if _, err := writeFrame(stream, finish, seq, nil); err != nil {
		return failure(ctx, err)
	}
	_, err := expect(stream, buf, finished, seq, nil, nil)
	return failure(ctx, err)
}

func runBulk(ctx context.Context, stream io.ReadWriter, buf, payload []byte, seq uint32, remaining int, result *Result) (uint32, error) {
	start := time.Now()
	defer func() {
		result.PayloadDuration = time.Since(start)
		if result.PayloadDuration > 0 {
			result.EchoBytesPerSecond = float64(result.EchoedBytes) / result.PayloadDuration.Seconds()
		}
	}()
	for remaining > 0 {
		p := payload[:min(remaining, len(payload))]
		fill(p, seq)
		if err := exchange(ctx, stream, buf, data, dataEcho, seq, p, result); err != nil {
			return seq, err
		}
		result.EchoedBytes += uint64(len(p))
		remaining -= len(p)
		seq++
	}
	return seq, nil
}

func exchange(ctx context.Context, stream io.ReadWriter, buf []byte, kind, echo byte, seq uint32, p []byte, result *Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	n, err := writeFrame(stream, kind, seq, p)
	result.BytesSent += uint64(n)
	if err != nil {
		return failure(ctx, err)
	}
	_, err = expect(stream, buf, echo, seq, p, &result.BytesReceived)
	return failure(ctx, err)
}

// Serve owns stream and enforces its budget from entry, including request reads
// and blocked echo writes. The request can shorten, never extend, that budget.
func Serve(ctx context.Context, stream io.ReadWriteCloser) error {
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, MaxDuration)
	defer cancel()
	defer guard(ctx, stream)()
	if err := ctx.Err(); err != nil {
		return err
	}
	buf := make([]byte, MaxFrameBytes)
	f, _, err := readFrame(stream, buf)
	if err != nil {
		return failure(ctx, err)
	}
	if f.kind != request || f.seq != 0 || len(f.payload) != 20 {
		return fmt.Errorf("%w: expected request", ErrProtocol)
	}
	// Validate unsigned wire values before converting to int on 32-bit hosts.
	samples := binary.BigEndian.Uint32(f.payload)
	total := binary.BigEndian.Uint64(f.payload[4:])
	frameBytes := binary.BigEndian.Uint32(f.payload[12:])
	millis := binary.BigEndian.Uint32(f.payload[16:])
	if samples < 1 || samples > MaxSamples || total < uint64(samples)*probeBytes || total > MaxPayloadBytes || frameBytes < probeBytes || frameBytes > maxDataBytes || millis < 1 || millis > uint32(MaxDuration/time.Millisecond) {
		return fmt.Errorf("%w: request exceeds limits", ErrProtocol)
	}
	budget, budgetCancel := context.WithDeadline(ctx, started.Add(time.Duration(millis)*time.Millisecond))
	defer budgetCancel()
	defer guard(budget, stream)()
	if err := budget.Err(); err != nil {
		return err
	}
	if _, err := writeFrame(stream, ready, 0, nil); err != nil {
		return failure(budget, err)
	}
	seq := uint32(1)
	for range samples {
		if err := echoFrame(budget, stream, buf, probe, probeEcho, seq, probeBytes); err != nil {
			return err
		}
		seq++
	}
	remaining := int(total) - int(samples)*probeBytes
	for remaining > 0 {
		n := min(remaining, int(frameBytes))
		if err := echoFrame(budget, stream, buf, data, dataEcho, seq, n); err != nil {
			return err
		}
		remaining -= n
		seq++
	}
	if _, err := expect(stream, buf, finish, seq, nil, nil); err != nil {
		return failure(budget, err)
	}
	_, err = writeFrame(stream, finished, seq, nil)
	return failure(budget, err)
}

func echoFrame(ctx context.Context, stream io.ReadWriter, buf []byte, kind, echo byte, seq uint32, size int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f, _, err := readFrame(stream, buf)
	if err != nil {
		return failure(ctx, err)
	}
	if f.kind != kind || f.seq != seq || len(f.payload) != size {
		return fmt.Errorf("%w: unexpected echo request", ErrProtocol)
	}
	_, err = writeFrame(stream, echo, seq, f.payload)
	return failure(ctx, err)
}

type frame struct {
	kind    byte
	seq     uint32
	payload []byte
}

func readFrame(r io.Reader, buf []byte) (frame, int, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return frame{}, 0, err
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if n < frameHeaderBytes || n > MaxFrameBytes-lengthPrefixBytes {
		return frame{}, 0, fmt.Errorf("%w: frame length %d", ErrProtocol, n)
	}
	read, err := io.ReadFull(r, buf[:n])
	if err != nil {
		return frame{}, max(0, read-frameHeaderBytes), err
	}
	return frame{buf[0], binary.BigEndian.Uint32(buf[1:5]), buf[5:n]}, int(n) - frameHeaderBytes, nil
}

func expect(r io.Reader, buf []byte, kind byte, seq uint32, payload []byte, received *uint64) (frame, error) {
	f, n, err := readFrame(r, buf)
	if received != nil {
		*received += uint64(n)
	}
	if err != nil {
		return frame{}, err
	}
	if f.kind != kind || f.seq != seq || !bytes.Equal(f.payload, payload) {
		return frame{}, fmt.Errorf("%w: unexpected echo response", ErrProtocol)
	}
	return f, nil
}

func writeFrame(w io.Writer, kind byte, seq uint32, payload []byte) (int, error) {
	var header [9]byte
	binary.BigEndian.PutUint32(header[:], uint32(frameHeaderBytes+len(payload)))
	header[4] = kind
	binary.BigEndian.PutUint32(header[5:], seq)
	if _, err := writeAll(w, header[:]); err != nil {
		return 0, err
	}
	return writeAll(w, payload)
}

func writeAll(w io.Writer, p []byte) (int, error) {
	total := 0
	for len(p) != 0 {
		n, err := w.Write(p)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

func fill(p []byte, seq uint32) {
	for i := range p {
		p[i] = byte(uint32(i) + seq)
	}
}

func failure(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, ErrProtocol) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrTransport, err)
}

// Joining an already-started callback prevents a canceled test from leaving a
// closer goroutine behind or racing cleanup of a reused concurrency slot.
func guard(ctx context.Context, stream io.Closer) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = stream.Close(); close(done) })
	return func() {
		if !stop() {
			<-done
		}
		_ = stream.Close()
	}
}
