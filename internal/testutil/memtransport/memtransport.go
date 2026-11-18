// Package memtransport provides unauthenticated in-process transports for tests.
package memtransport

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

type openRequest struct {
	ctx    context.Context
	result chan openResult
}
type openResult struct {
	stream transport.Stream
	err    error
}

type session struct {
	opens chan openRequest
	mux   *yamux.Session
	peer  identity.Fingerprint
	done  chan struct{}
	once  sync.Once
	err   error
}

// Pair treats a as the dialer and returns sessions whose Peer is the other ID.
func Pair(t testing.TB, a, b identity.Fingerprint) (transport.Session, transport.Session) {
	t.Helper()
	left, right := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = false
	cfg.StreamCloseTimeout = 0
	cfg.StreamOpenTimeout = 0
	cfg.LogOutput = io.Discard
	x, err := yamux.Client(left, cfg)
	if err != nil {
		t.Fatal(err)
	}
	y, err := yamux.Server(right, cfg)
	if err != nil {
		t.Fatal(err)
	}
	wrap := func(m *yamux.Session, p identity.Fingerprint) *session {
		s := &session{mux: m, peer: p, done: make(chan struct{}), opens: make(chan openRequest)}
		go func() { <-m.CloseChan(); s.terminate(io.EOF) }()
		go s.openLoop()
		t.Cleanup(func() { _ = s.Close(nil); <-s.Done() })
		return s
	}
	return wrap(x, b), wrap(y, a)
}

// One session-owned worker bounds pending yamux opens. Canceling a request
// abandons only that stream; the worker is released by ack or session shutdown.
func (s *session) openLoop() {
	for {
		select {
		case <-s.done:
			return
		case req := <-s.opens:
			if req.ctx.Err() != nil {
				continue
			}
			st, err := s.mux.OpenStream()
			var wrapped transport.Stream
			if st != nil {
				wrapped = &stream{st}
			}
			select {
			case req.result <- openResult{wrapped, err}:
			case <-req.ctx.Done():
				if wrapped != nil {
					_ = wrapped.Close()
				}
			case <-s.done:
				if wrapped != nil {
					_ = wrapped.Close()
				}
				return
			}
		}
	}
}
func (s *session) OpenStream(ctx context.Context) (transport.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	req := openRequest{ctx: ctx, result: make(chan openResult)}
	select {
	case s.opens <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.Err()
	}
	select {
	case result := <-req.result:
		if err := ctx.Err(); err != nil {
			if result.stream != nil {
				_ = result.stream.Close()
			}
			return nil, err
		}
		return result.stream, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.Err()
	}
}
func (s *session) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.mux.AcceptStreamWithContext(ctx)
	if err != nil {
		return nil, err
	}
	return &stream{st}, nil
}
func (s *session) Peer() identity.Fingerprint { return s.peer }
func (s *session) Path() transport.PathInfo   { return transport.PathInfo{Kind: transport.PathDirect} }
func (s *session) Stats() transport.LinkStats { return transport.LinkStats{} }
func (s *session) terminate(reason error) {
	if reason == nil {
		reason = io.EOF
	}
	s.once.Do(func() { s.err = reason; close(s.done) })
}
func (s *session) Close(reason error) error {
	s.terminate(reason)
	return s.mux.Close()
}
func (s *session) Done() <-chan struct{} { return s.done }
func (s *session) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}

type stream struct{ *yamux.Stream }

func (s *stream) CloseWrite() error { return s.Stream.Close() }
func (s *stream) Close() error {
	_ = s.Stream.SetDeadline(time.Now())
	return s.Stream.Close()
}
