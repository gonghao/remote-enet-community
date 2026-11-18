// Package app owns client and operator lifetimes independently of the CLI.
package app

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/identity"
	"remote-enet/internal/observe"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
)

type Phase string

const (
	PhaseIdle           Phase = "idle"
	PhaseStarting       Phase = "starting"
	PhaseWaitingNIC     Phase = "waiting-nic"
	PhaseDiscovering    Phase = "discovering"
	PhaseWaitingVehicle Phase = "waiting-vehicle"
	PhaseConnecting     Phase = "connecting"
	PhaseListening      Phase = "listening"
	PhaseConnected      Phase = "connected"
	PhaseDisconnected   Phase = "disconnected"
	PhaseBackoff        Phase = "backoff"
	PhaseStopped        Phase = "stopped"
)

var (
	ErrNotConnected = errors.New("app: no established session")
	ErrAlreadyRun   = errors.New("app: runtime can only run once")
)

// Snapshot owns its metrics collections and excludes credentials and tunnel
// bytes. Generation identifies a successfully handshaken session. RTTValid
// distinguishes an unmeasured RTT from a measured duration, including zero.
type Snapshot struct {
	Role       session.Role
	Phase      Phase
	Device     identity.Fingerprint
	NIC        carside.NIC
	Gateway    netip.Addr
	VIN        string
	Peer       identity.Fingerprint
	Path       transport.PathInfo
	Generation uint64
	LastError  string
	NextRetry  time.Time
	RTT        time.Duration
	RTTValid   bool
	Diagnostic netip.AddrPort
	Control    netip.AddrPort
	Discovery  netip.AddrPort
	Metrics    observe.Snapshot
	// MetricsIncomplete also applies to lifetime totals after reconnecting.
	MetricsIncomplete bool
	Exclusive         exclusive.Snapshot
	Direct            DirectState
}

type subscription struct {
	ch   chan Snapshot
	stop func() bool
}

type activeSession struct {
	s   *session.Session
	ctx context.Context
}

// Runtime is single-use. RunClient or RunOperator owns all network resources;
// Snapshot, Subscribe and WithSession may run concurrently with it.
type Runtime struct {
	mu             sync.Mutex
	state          Snapshot
	started        bool
	stopped        bool
	active         *activeSession
	subs           map[*subscription]struct{}
	metrics        *observe.Tracker
	exclusiveState func() exclusive.Snapshot
	direct         *directManager
}

// New creates an idle runtime; configuration is supplied to its run method.
func New() *Runtime {
	return &Runtime{state: Snapshot{Phase: PhaseIdle, Direct: DirectState{Phase: DirectDisabled}}, subs: make(map[*subscription]struct{}), metrics: observe.New(observe.Options{})}
}

// Snapshot is independent of future updates and safe for callers to modify.
func (r *Runtime) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshotLocked()
}

func (r *Runtime) snapshotLocked() Snapshot {
	s := r.state
	if r.metrics != nil {
		s.Metrics = r.metrics.Snapshot()
		if r.active != nil && s.Metrics.Session.Active {
			s.RTT = s.Metrics.Session.RTT.Latest
			s.RTTValid = s.Metrics.Session.RTT.TotalSamples > 0
		}
	}
	if r.exclusiveState != nil {
		s.Exclusive = r.exclusiveState()
	}
	return s
}

// Subscribe immediately delivers the current snapshot. Capacity is one;
// newer updates replace unread updates. Cancellation or runtime exit closes
// the channel without a per-subscriber goroutine.
func (r *Runtime) Subscribe(ctx context.Context) <-chan Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	sub := &subscription{ch: make(chan Snapshot, 1)}
	if ctx.Err() != nil {
		close(sub.ch)
		return sub.ch
	}
	sub.ch <- r.snapshotLocked()
	if r.stopped {
		close(sub.ch)
		return sub.ch
	}
	if r.subs == nil {
		r.subs = make(map[*subscription]struct{})
	}
	r.subs[sub] = struct{}{}
	sub.stop = context.AfterFunc(ctx, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[sub]; ok {
			delete(r.subs, sub)
			close(sub.ch)
		}
	})
	return sub.ch
}

// WithSession borrows the current generation for the duration of fn. The
// callback must honor ctx and must not retain or close the session. Its context
// is canceled when either the caller or that generation ends; it never switches
// an in-flight operation onto a replacement connection.
func (r *Runtime) WithSession(ctx context.Context, fn func(context.Context, uint64, *session.Session) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	active, generation := r.active, r.state.Generation
	r.mu.Unlock()
	if active == nil || active.ctx.Err() != nil {
		return ErrNotConnected
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(active.ctx, cancel)
	defer stop()
	if active.ctx.Err() != nil {
		return ErrNotConnected
	}
	if err := fn(opCtx, generation, active.s); err != nil {
		return err
	}
	if active.ctx.Err() != nil {
		cancel()
	}
	return opCtx.Err()
}

func (r *Runtime) begin(role session.Role) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.started {
		return ErrAlreadyRun
	}
	r.started = true
	if r.metrics == nil {
		r.metrics = observe.New(observe.Options{})
	}
	r.state.Role, r.state.Phase = role, PhaseStarting
	r.publishLocked()
	return nil
}

func (r *Runtime) finish(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Phase, r.state.NextRetry = PhaseStopped, time.Time{}
	if err != nil && !errors.Is(err, context.Canceled) {
		r.state.LastError = err.Error()
	}
	r.stopped = true
	r.publishLocked()
	for sub := range r.subs {
		sub.stop()
		close(sub.ch)
		delete(r.subs, sub)
	}
}

func (r *Runtime) update(fn func(*Snapshot)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.state)
	r.publishLocked()
}

func (r *Runtime) publishLocked() {
	for sub := range r.subs {
		select {
		case <-sub.ch:
		default:
		}
		// Each subscriber owns its metrics slices independently of every other
		// subscriber, as well as independently of the tracker.
		sub.ch <- r.snapshotLocked()
	}
}

func (r *Runtime) activate(ctx context.Context, s *session.Session, path transport.PathInfo) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = &activeSession{s: s, ctx: ctx}
	r.state.Phase, r.state.Path, r.state.Peer = PhaseConnected, path, s.Peer()
	r.state.NextRetry = time.Time{}
	r.publishLocked()
	return r.state.Generation
}

func (r *Runtime) deactivate() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = nil
	r.exclusiveState = nil
	r.state.Phase = PhaseDisconnected
	r.state.Path = transport.PathInfo{}
	r.state.RTT, r.state.RTTValid = 0, false
	r.state.Diagnostic, r.state.Control, r.state.Discovery = netip.AddrPort{}, netip.AddrPort{}, netip.AddrPort{}
	r.publishLocked()
}

func pause(ctx context.Context, duration time.Duration) error {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
