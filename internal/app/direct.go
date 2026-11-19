package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/l3"
	"remote-enet/internal/mgu"
	"remote-enet/internal/operator"
	"remote-enet/internal/packetio"
	"remote-enet/internal/session"
	"remote-enet/internal/tunadapter"
	"remote-enet/internal/tunnel"
)

// DirectPhase describes the session-owned direct link, independently of HSFZ health.
type DirectPhase string

const (
	DirectDisabled    DirectPhase = "disabled"
	DirectDiscovering DirectPhase = "discovering"
	DirectStarting    DirectPhase = "starting"
	DirectReady       DirectPhase = "ready"
	DirectStopping    DirectPhase = "stopping"
	DirectFailed      DirectPhase = "failed"
)

// DirectState belongs to one session generation and contains no diagnostic bytes.
type DirectState struct {
	Phase     DirectPhase
	Address   netip.Addr
	Local     netip.Addr
	Adapter   string
	LastError string
}

// DirectDevice owns only the interface and routes created by its factory.
type DirectDevice interface {
	packetio.Device
	Name() string
	Local() netip.Prefix
}

// DirectFactory must clean up partial resources on error and honor cancellation.
// The supplied context owns the device until the direct link is disabled.
type DirectFactory func(context.Context, []netip.Addr) (DirectDevice, error)

type directManager struct {
	closeMu     sync.Mutex
	draining    bool
	runtime     *Runtime
	ctx         context.Context
	cancel      context.CancelFunc
	presenter   *operator.Presenter
	opener      *session.Session
	queryOpener mgu.Opener
	factory     DirectFactory
	observer    tunnel.Observer
	gate        *exclusive.Gate
	port        uint16
	mu          sync.Mutex
	stopped     bool
	cancelOp    context.CancelFunc
	done        chan struct{}
	device      DirectDevice
	evo         bool
	stack       *l3.Stack
	closeErr    error
}

func (m *directManager) publish(state DirectState) {
	r := m.runtime
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.direct == m {
		r.state.Direct = state
		r.publishLocked()
	}
}

// ConnectMGU rejects queued operations targeting an obsolete session generation.
// Caller cancellation applies to setup; a successful link belongs to the session.
func (r *Runtime) ConnectMGU(ctx context.Context, expectedGen uint64) error {
	m, err := r.directForGeneration(expectedGen)
	if err != nil {
		return err
	}
	return m.connect(ctx, false)
}

// ConnectEVO selects a reachable SSH endpoint from the known EVO addresses.
func (r *Runtime) ConnectEVO(ctx context.Context, expectedGen uint64) error {
	m, err := r.directForGeneration(expectedGen)
	if err != nil {
		return err
	}
	return m.connect(ctx, true)
}

// DisconnectMGU cancels and joins setup before releasing the link's resources.
func (r *Runtime) DisconnectMGU(ctx context.Context, expectedGen uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := r.directForGeneration(expectedGen)
	if err != nil {
		return err
	}
	return m.disconnect(false)
}

func (r *Runtime) directForGeneration(expected uint64) (*directManager, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil || r.active.ctx.Err() != nil || r.direct == nil || expected != 0 && expected != r.state.Generation {
		return nil, ErrNotConnected
	}
	return r.direct, nil
}

func (m *directManager) connect(ctx context.Context, evo bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := m.checkHealth(); err != nil {
		return err
	}
	m.mu.Lock()
	if m.stopped || m.draining || m.ctx.Err() != nil {
		m.mu.Unlock()
		return ErrNotConnected
	}
	if m.done != nil {
		m.mu.Unlock()
		return exclusive.ErrBusy
	}
	if m.device != nil {
		if m.evo != evo {
			m.mu.Unlock()
			return exclusive.ErrBusy
		}
		select {
		case <-m.stack.Done():
			m.mu.Unlock()
			if err := m.checkHealth(); err != nil {
				return err
			}
			return fmt.Errorf("direct: L3 stopped unexpectedly")
		default:
		}
		m.mu.Unlock()
		return nil
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(m.ctx, cancel)
	done := make(chan struct{})
	m.cancelOp, m.done = cancel, done
	m.mu.Unlock()
	defer func() {
		stop()
		cancel()
		m.mu.Lock()
		m.cancelOp = nil
		m.done = nil
		close(done)
		m.mu.Unlock()
	}()
	state := DirectState{Phase: DirectDiscovering}
	m.publish(state)
	var device DirectDevice
	var stack *l3.Stack
	// The resource context outlives the caller's setup context after commit.
	linkCtx, cancelLink := context.WithCancel(context.WithoutCancel(m.ctx))
	stopSetup := context.AfterFunc(opCtx, cancelLink)
	var address netip.Addr
	err := m.presenter.WithMaintenance(opCtx, func(ctx context.Context, gateway netip.Addr) error {
		if evo {
			var err error
			address, err = discoverEVO(ctx, m.opener)
			return err
		}
		info, err := mgu.Discover(ctx, m.queryOpener, netip.AddrPortFrom(gateway, m.port), mgu.Options{})
		if err == nil {
			address = info.Address
		}
		return err
	})
	if err == nil {
		err = opCtx.Err()
	}
	if err == nil {
		state.Address, state.Phase = address, DirectStarting
		m.publish(state)
		device, err = m.factory(linkCtx, []netip.Addr{address})
	}
	if err == nil {
		err = opCtx.Err()
	}
	if err == nil {
		opts := l3.Options{Targets: []netip.Addr{address}, Opener: m.opener, Observer: m.observer}
		if evo || m.gate != nil {
			opts.Admit = func(ctx context.Context, local, remote netip.AddrPort) (func(), error) {
				if evo && local.Port() != 22 {
					return nil, fmt.Errorf("evo: only TCP port 22 is supported")
				}
				if m.gate == nil {
					return nil, nil
				}
				lease, err := m.gate.Admit(ctx, local, remote)
				if err != nil {
					return nil, err
				}
				return lease.Release, nil
			}
		}
		stack, err = l3.Start(linkCtx, device, opts)
	}

	m.mu.Lock()
	if err == nil {
		err = opCtx.Err()
	}
	if err == nil {
		select {
		case <-stack.Done():
			err = stack.Err()
			if err == nil {
				err = fmt.Errorf("direct: L3 stopped during setup")
			}
		default:
		}
	}
	if err == nil && (m.stopped || m.ctx.Err() != nil) {
		err = context.Canceled
	}
	if err == nil && !stopSetup() {
		err = context.Canceled
	}
	if err == nil {
		m.device, m.stack, m.evo = device, stack, evo
		// The session manager explicitly closes stack before device.
		m.cancel = cancelLink
	}
	m.mu.Unlock()
	if err != nil {
		stopSetup()
		var cleanupErr error
		if stack != nil {
			cleanupErr = errors.Join(cleanupErr, stack.Close())
		}
		if device != nil {
			cleanupErr = errors.Join(cleanupErr, device.Close())
		}
		cancelLink()
		if cleanupErr != nil {
			m.mu.Lock()
			m.closeErr = cleanupErr
			m.stopped = true
			m.mu.Unlock()
		}
		err = errors.Join(err, cleanupErr)
		state.Phase, state.LastError = DirectFailed, err.Error()
		m.publish(state)
		return err
	}
	state.Phase, state.Local, state.Adapter = DirectReady, device.Local().Addr(), device.Name()
	m.publish(state)
	return nil
}

// checkHealth shares teardown serialization with Disable and session replacement.
// It creates no worker and cannot apply a delayed result to a replacement stack.
func (m *directManager) checkHealth() error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.stack == nil || m.draining || m.done != nil {
		return nil
	}
	select {
	case <-m.stack.Done():
	default:
		return nil
	}
	if m.ctx.Err() != nil {
		return nil
	}
	failure := m.stack.Err()
	if failure == nil {
		failure = fmt.Errorf("direct: L3 stopped unexpectedly")
	}
	m.closeErr = errors.Join(m.closeErr, m.stack.Close(), m.device.Close())
	m.cancel()
	m.stack, m.device = nil, nil
	if m.closeErr != nil {
		m.stopped = true
	}
	failure = errors.Join(failure, m.closeErr)
	m.publish(DirectState{Phase: DirectFailed, LastError: failure.Error()})
	return failure
}

func (m *directManager) disconnect(stop bool) error {
	m.closeMu.Lock()
	defer m.closeMu.Unlock()
	m.mu.Lock()
	m.draining = true
	if stop {
		m.stopped = true
	}
	if m.cancelOp != nil {
		m.cancelOp()
	}
	done := m.done
	m.mu.Unlock()
	if done != nil {
		<-done
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	defer func() { m.draining = false }()
	if m.device == nil {
		if m.closeErr == nil {
			m.publish(DirectState{Phase: DirectDisabled})
		}
		return m.closeErr
	}
	m.publish(DirectState{Phase: DirectStopping})
	m.closeErr = errors.Join(m.closeErr, m.stack.Close(), m.device.Close())
	m.cancel()
	m.stack, m.device = nil, nil
	state := DirectState{Phase: DirectDisabled}
	if m.closeErr != nil {
		m.stopped = true
		state.Phase, state.LastError = DirectFailed, m.closeErr.Error()
	}
	m.publish(state)
	return m.closeErr
}

func nativeDirectFactory(ctx context.Context, targets []netip.Addr) (DirectDevice, error) {
	device, err := tunadapter.OpenAuto(ctx, targets)
	if device == nil {
		return nil, err
	}
	return device, err
}
