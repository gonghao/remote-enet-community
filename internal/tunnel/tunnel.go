// Package tunnel relays opaque bytes without inspecting their contents.
package tunnel

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"time"

	"remote-enet/internal/transport"
)

// Stats counts bytes received from a (In) and sent to a (Out).
// StartedAt must only be read after Pipe returns or externally synchronized.
type Stats struct {
	BytesIn, BytesOut atomic.Uint64
	StartedAt         time.Time
	// Counter receives successful write increments, in the same directions as
	// BytesIn/BytesOut. It must be concurrency-safe and must not perform I/O.
	Counter Counter
	Payload Tap
}

// Tap observes successful destination writes in Counter directions. Implementations
// must be concurrency-safe, return immediately, and copy retained bytes.
type Tap interface {
	In([]byte)
	Out([]byte)
}

// Counter measures successful payload writes, excluding stream headers.
type Counter interface {
	AddIn(uint64)
	AddOut(uint64)
}

// Activity covers one admitted forwarding attempt, including open failures.
type Activity interface {
	Counter
	End(error)
}

// Observer must return promptly. A nil Activity disables observation for that
// attempt without rejecting or otherwise changing the forwarding operation.
type Observer interface {
	Begin(target string) Activity
}

// Pipe owns both endpoints. A clean EOF only closes the destination's write
// half; both endpoints close after both copies finish, an error, or cancellation.
// b must support CloseWrite; otherwise no bytes are copied.
func Pipe(ctx context.Context, a transport.Stream, b net.Conn, st *Stats) error {
	defer a.Close()
	defer b.Close()
	bw, ok := b.(interface{ CloseWrite() error })
	if !ok {
		return fmt.Errorf("tunnel: connection does not support CloseWrite: %w", transport.ErrUnsupported)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if st != nil {
		st.StartedAt = time.Now()
	}
	results := make(chan error, 2)
	copyOne := func(dst io.Writer, src io.Reader, closeWrite func() error, count *atomic.Uint64, add func(uint64), payload func([]byte)) {
		if count != nil || add != nil || payload != nil {
			dst = countingWriter{dst, count, add, payload}
		}
		_, err := io.Copy(dst, src)
		if err == nil {
			err = closeWrite()
		}
		results <- err
	}
	var in, out *atomic.Uint64
	var addIn, addOut func(uint64)
	var payloadIn, payloadOut func([]byte)
	if st != nil {
		in, out = &st.BytesIn, &st.BytesOut
		if st.Payload != nil {
			payloadIn, payloadOut = st.Payload.In, st.Payload.Out
		}
		if st.Counter != nil {
			addIn, addOut = st.Counter.AddIn, st.Counter.AddOut
		}
	}
	go copyOne(b, a, bw.CloseWrite, in, addIn, payloadIn)
	go copyOne(a, b, a.CloseWrite, out, addOut, payloadOut)
	var first error
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil && first == nil {
				first = fmt.Errorf("tunnel: copy: %w", err)
				_ = a.Close()
				_ = b.Close()
			}
		case <-ctx.Done():
			first = ctx.Err()
			_ = a.Close()
			_ = b.Close()
			// Drain all outstanding workers before returning ownership to the caller.
			for ; i < 2; i++ {
				<-results
			}
			return first
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return first
}

type countingWriter struct {
	io.Writer
	count   *atomic.Uint64
	add     func(uint64)
	payload func([]byte)
}

func (w countingWriter) Write(p []byte) (int, error) {
	n, err := w.Writer.Write(p)
	if w.count != nil {
		w.count.Add(uint64(n))
	}
	if w.add != nil && n > 0 {
		w.add(uint64(n))
	}
	if w.payload != nil && n > 0 {
		w.payload(p[:n])
	}
	return n, err
}
