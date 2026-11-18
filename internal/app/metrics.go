package app

import (
	"context"
	"errors"
	"sync"
	"time"

	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/session"
	"remote-enet/internal/tunnel"
)

// measurements binds callbacks to one connection, including callbacks that
// race with Run returning or teardown. It cannot start a generation after End.
type measurements struct {
	runtime    *Runtime
	direction  observe.Direction
	mu         sync.Mutex
	generation uint64
	ended      bool
}

func (m *measurements) start() uint64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ended {
		return 0
	}
	if m.generation == 0 {
		r := m.runtime
		r.mu.Lock()
		m.generation = r.metrics.BeginSession()
		r.state.Generation = m.generation
		r.mu.Unlock()
	}
	return m.generation
}

func (m *measurements) Begin(target string) tunnel.Activity {
	generation := m.start()
	if generation == 0 {
		return nil
	}
	handle, err := m.runtime.metrics.BeginTunnel(generation, target, m.direction)
	if err != nil {
		// Observation is best effort: a bounded tracker cannot impose a new
		// connection limit on opaque forwarding. The event marks partial data.
		m.runtime.metrics.RecordEvent(generation, "metrics-incomplete", target, err)
		m.runtime.mu.Lock()
		if m.runtime.state.Generation == generation {
			m.runtime.state.MetricsIncomplete = true
		}
		m.runtime.mu.Unlock()
		return nil
	}
	return handle
}

func (m *measurements) onRTT(sample time.Duration) {
	if generation := m.start(); generation != 0 {
		m.runtime.metrics.AddRTT(generation, sample)
	}
}

func (m *measurements) end(err error) {
	m.mu.Lock()
	m.ended = true
	generation := m.generation
	m.mu.Unlock()
	if generation != 0 {
		m.runtime.metrics.EndSession(generation, err)
	}
}

// TestLink borrows exactly one generation and exchanges only peer-test data.
// A partial result can accompany an error; cancellation never closes the session.
func (r *Runtime) TestLink(ctx context.Context, options linktest.Options) (linktest.Result, error) {
	return r.TestLinkForGeneration(ctx, 0, options)
}

// TestLinkForGeneration rejects an obsolete queued request before testing a
// replacement session. Zero selects the current generation, as TestLink does.
func (r *Runtime) TestLinkForGeneration(ctx context.Context, expected uint64, options linktest.Options) (result linktest.Result, err error) {
	started := time.Now()
	var generation uint64
	err = r.WithSession(ctx, func(ctx context.Context, gen uint64, s *session.Session) error {
		if expected != 0 && gen != expected {
			return ErrNotConnected
		}
		generation = gen
		var testErr error
		result, testErr = s.TestLink(ctx, options)
		return testErr
	})
	result.Duration = time.Since(started)
	result.TimedOut = errors.Is(err, context.DeadlineExceeded)
	result.Interrupted = err != nil && !result.TimedOut
	if generation != 0 {
		r.metrics.RecordEvent(generation, "link-test", "peer-only echo", err)
	}
	return result, err
}
