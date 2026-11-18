package app

import (
	"context"
	"io"
	"net"
	"sync"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/operator"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

// captureQueryOpener is used exclusively by the internal read-only MGU query.
type captureQueryOpener struct {
	opener     operator.Opener
	recorder   *hsfzcapture.Recorder
	generation func() uint64
}

func (o captureQueryOpener) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	var capture *hsfzcapture.Connection
	if o.recorder != nil {
		var generation uint64
		if o.generation != nil {
			generation = o.generation()
		}
		capture = o.recorder.Begin(hsfzcapture.Metadata{Generation: generation, Network: "tcp", Channel: "mgu-query", Local: "internal", Peer: h.Target, Target: h.Target})
	}
	st, err := o.opener.OpenTunnel(ctx, h)
	if err != nil {
		if capture != nil {
			capture.End(err)
		}
		return nil, err
	}
	if capture == nil {
		return st, nil
	}
	return &captureQueryStream{Stream: st, capture: capture}, nil
}

type captureQueryStream struct {
	transport.Stream
	capture  *hsfzcapture.Connection
	mu       sync.Mutex
	closed   bool
	err      error
	workers  sync.WaitGroup
	once     sync.Once
	closeErr error
}

func (s *captureQueryStream) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.workers.Add(1)
	return true
}
func (s *captureQueryStream) finish(err error) {
	if err != nil && err != io.EOF {
		s.mu.Lock()
		if s.err == nil {
			s.err = err
		}
		s.mu.Unlock()
	}
	s.workers.Done()
}
func (s *captureQueryStream) Read(p []byte) (int, error) {
	if !s.begin() {
		return 0, net.ErrClosed
	}
	n, err := s.Stream.Read(p)
	if n > 0 {
		s.capture.Record(hsfzcapture.FromVehicle, p[:n])
	}
	s.finish(err)
	return n, err
}
func (s *captureQueryStream) Write(p []byte) (int, error) {
	if !s.begin() {
		return 0, net.ErrClosed
	}
	n, err := s.Stream.Write(p)
	if n > 0 {
		s.capture.Record(hsfzcapture.ToVehicle, p[:n])
	}
	s.finish(err)
	return n, err
}
func (s *captureQueryStream) CloseWrite() error {
	if !s.begin() {
		return net.ErrClosed
	}
	err := s.Stream.CloseWrite()
	s.finish(err)
	return err
}
func (s *captureQueryStream) Close() error {
	s.once.Do(func() {
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		s.closeErr = s.Stream.Close()
		s.workers.Wait()
		s.mu.Lock()
		err := s.err
		s.mu.Unlock()
		if err == nil {
			err = s.closeErr
		}
		s.capture.End(err)
	})
	return s.closeErr
}
