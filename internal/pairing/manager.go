// Package pairing owns the ephemeral numeric code for one operator listener.
package pairing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"remote-enet/internal/pairing/cpace"
)

var (
	ErrAlreadyRedeeming = errors.New("already_redeeming")
	ErrRedeemed         = errors.New("redeemed")
)

// Config.Address is the validated share address. Zero limits select the protocol defaults.
type Config struct {
	Address                  string
	TTL, Window, SaveTimeout time.Duration
	PerCode, GlobalLimit     int
	Now                      func() time.Time
	// AfterFunc schedules expiry on the same clock as Now and returns a stop function.
	AfterFunc func(time.Duration, func()) func()
}

// Status is safe to include in general application snapshots.
type Status struct {
	Generation   uint64
	Address      string
	ExpiresAt    time.Time
	AttemptsLeft int
	Committing   bool
	Reason       cpace.Reason
}

// Display is for explicit presentation of the current generation only.
type Display struct {
	Status
	Code string
}

// Manager serializes authorization with generation changes, but never with disk I/O.
type Manager struct {
	mu               sync.Mutex
	cfg              Config
	status           Status
	code             string
	charges          []time.Time
	pending          map[*Attempt]struct{}
	owner            *Attempt
	stopTimer        func()
	saving           chan struct{}
	commitGeneration uint64
	redeemed, closed bool
}

// NewManager starts without an available code.
func NewManager(cfg Config) *Manager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.AfterFunc == nil {
		cfg.AfterFunc = func(d time.Duration, f func()) func() {
			timer := time.AfterFunc(d, f)
			return func() { timer.Stop() }
		}
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 5 * time.Minute
	}
	if cfg.Window <= 0 {
		cfg.Window = 15 * time.Minute
	}
	if cfg.SaveTimeout <= 0 {
		cfg.SaveTimeout = 3 * time.Second
	}
	if cfg.PerCode <= 0 {
		cfg.PerCode = 5
	}
	if cfg.GlobalLimit <= 0 {
		cfg.GlobalLimit = 20
	}
	return &Manager{cfg: cfg, status: Status{Address: cfg.Address, Reason: cpace.ReasonUnavailable}, pending: make(map[*Attempt]struct{})}
}

func (m *Manager) refresh() time.Time {
	now := m.cfg.Now()
	for len(m.charges) > 0 && !now.Before(m.charges[0].Add(m.cfg.Window)) {
		m.charges = m.charges[1:]
	}
	if m.code != "" && !now.Before(m.status.ExpiresAt) {
		m.retire(cpace.ReasonExpired)
	}
	return now
}

func (m *Manager) retire(reason cpace.Reason) {
	if m.stopTimer != nil {
		m.stopTimer()
		m.stopTimer = nil
	}
	for a := range m.pending {
		a.detach()
		a.cancel()
		delete(m.pending, a)
	}
	m.owner = nil
	m.code = ""
	m.status.AttemptsLeft = 0
	m.status.Reason = reason
}

// Issue replaces any uncommitted generation.
func (m *Manager) Issue() (Display, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.refresh()
	if m.closed {
		return Display{}, cpace.ReasonUnavailable
	}
	if len(m.charges) >= m.cfg.GlobalLimit {
		return Display{}, cpace.ReasonRateLimited
	}
	if m.saving != nil {
		return Display{}, cpace.ReasonBusy
	}
	m.retire(cpace.ReasonUnavailable)
	n, err := rand.Int(rand.Reader, big.NewInt(100000000))
	if err != nil {
		return Display{}, fmt.Errorf("generate pairing code: %w", err)
	}
	gen := m.status.Generation + 1
	m.status = Status{Generation: gen, Address: m.cfg.Address, ExpiresAt: now.Add(m.cfg.TTL), AttemptsLeft: m.cfg.PerCode}
	m.redeemed = false
	m.code = fmt.Sprintf("%08d", n.Int64())
	m.stopTimer = m.cfg.AfterFunc(m.status.ExpiresAt.Sub(m.cfg.Now()), func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if m.status.Generation == gen {
			m.refresh()
		}
	})
	return Display{m.status, m.code}, nil
}

// Status observes expiry even when a timer callback is delayed.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	return m.status
}

// Display checks the generation supplied by the explicit copy operation.
func (m *Manager) Display(generation uint64) (Display, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	if m.code == "" || generation != m.status.Generation {
		return Display{}, cpace.ReasonUnavailable
	}
	return Display{m.status, m.code}, nil
}

// Revoke reports an irreversible save immediately.
func (m *Manager) Revoke(ctx context.Context, generation uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	if generation == m.commitGeneration && m.saving != nil {
		return ErrAlreadyRedeeming
	}
	if generation == m.commitGeneration && m.redeemed {
		return ErrRedeemed
	}
	if generation != m.status.Generation || m.closed {
		return cpace.ReasonUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if m.code == "" {
		return m.status.Reason
	}
	m.retire(cpace.ReasonUnavailable)
	return nil
}

// Attempt owns no secret and can charge at most one ShareA.
type Attempt struct {
	m                     *Manager
	generation            uint64
	parent                context.Context
	ctx                   context.Context
	cancel                context.CancelFunc
	detach                func() bool
	lastGlobal, responded bool
	committing            bool
}

// Context is canceled if a pending handshake loses its generation.
func (a *Attempt) Context() context.Context { return a.ctx }

func (m *Manager) admission() error {
	if m.closed {
		return cpace.ReasonUnavailable
	}
	if m.code == "" && m.saving == nil {
		return m.status.Reason
	}
	if len(m.charges) >= m.cfg.GlobalLimit {
		return cpace.ReasonRateLimited
	}
	if m.saving != nil || m.owner != nil {
		return cpace.ReasonBusy
	}
	return nil
}

// Begin binds the handshake to the current generation without consuming a guess.
func (m *Manager) Begin(ctx context.Context) (*Attempt, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := m.admission(); err != nil {
		return nil, err
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	a := &Attempt{m: m, generation: m.status.Generation, parent: ctx, ctx: attemptCtx, cancel: cancel}
	a.detach = context.AfterFunc(attemptCtx, a.Finish)
	m.pending[a] = struct{}{}
	return a, nil
}

// Respond charges both budgets before point validation.
func (a *Attempt) Respond(b cpace.Binding, share [32]byte) (*cpace.Responder, [32]byte, error) {
	m := a.m
	code, err := a.charge()
	if err != nil {
		return nil, [32]byte{}, err
	}
	r, y, err := cpace.Respond(code, b, share)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refresh()
	if m.owner != a {
		return nil, [32]byte{}, a.inactiveReason()
	}
	if err != nil {
		// Leave the stream usable for the caller to send the rejection before Finish.
		a.releaseLocked()
		return nil, [32]byte{}, cpace.ReasonConfirmation
	}
	if err := a.ctx.Err(); err != nil {
		a.cancel()
		a.releaseLocked()
		return nil, [32]byte{}, err
	}
	a.responded = true
	return r, y, nil
}

func (a *Attempt) inactiveReason() cpace.Reason {
	m := a.m
	if a.generation == m.status.Generation && m.status.Reason != "" {
		return m.status.Reason
	}
	return cpace.ReasonUnavailable
}

func (a *Attempt) charge() (string, error) {
	m := a.m
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.refresh()
	if _, ok := m.pending[a]; !ok || a.generation != m.status.Generation {
		return "", a.inactiveReason()
	}
	if err := a.ctx.Err(); err != nil {
		return "", err
	}
	if err := m.admission(); err != nil {
		return "", err
	}
	m.charges = append(m.charges, now)
	m.status.AttemptsLeft--
	a.lastGlobal = len(m.charges) == m.cfg.GlobalLimit
	m.owner = a
	return m.code, nil
}

func (a *Attempt) releaseLocked() {
	m := a.m
	if _, ok := m.pending[a]; !ok {
		return
	}
	delete(m.pending, a)
	a.detach()
	if m.owner != a {
		return
	}
	m.owner = nil
	if m.status.AttemptsLeft == 0 {
		m.retire(cpace.ReasonExhausted)
	} else if a.lastGlobal {
		m.retire(cpace.ReasonRateLimited)
	}
}

// Finish releases only a pending handshake. The save owns its slot until it returns.
func (a *Attempt) Finish() {
	a.m.mu.Lock()
	defer a.m.mu.Unlock()
	if a.committing {
		return
	}
	a.cancel()
	a.releaseLocked()
}

// Abort abandons the attempt without refunding an admitted guess.
func (a *Attempt) Abort() { a.Finish() }

// Commit requires the caller to have verified the client confirmation.
// Only errors returned by save become ReasonStoreFailed.
func (a *Attempt) Commit(save func(context.Context) error) error {
	m := a.m
	m.mu.Lock()
	m.refresh()
	// The cancellation callback may already have released the pending slot.
	if err := a.parent.Err(); err != nil && !a.committing {
		a.cancel()
		a.releaseLocked()
		m.mu.Unlock()
		return err
	}
	if m.owner != a || !a.responded || a.generation != m.status.Generation {
		reason := a.inactiveReason()
		m.mu.Unlock()
		return reason
	}
	a.committing = true
	delete(m.pending, a)
	a.detach()
	m.retire(cpace.ReasonUnavailable)
	m.commitGeneration = a.generation
	m.status.Committing = true
	m.saving = make(chan struct{})
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), m.cfg.SaveTimeout)
	err := save(ctx)
	cancel()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.Committing = false
	m.redeemed = err == nil
	close(m.saving)
	m.saving = nil
	if err != nil {
		m.status.Reason = cpace.ReasonStoreFailed
		return cpace.ReasonStoreFailed
	}
	return nil
}

// Close cancels uncommitted handshakes and waits for any authorized save.
func (m *Manager) Close() {
	m.mu.Lock()
	if !m.closed {
		m.closed = true
		m.retire(cpace.ReasonUnavailable)
		m.status.Generation++
		m.redeemed = false
	}
	saving := m.saving
	m.mu.Unlock()
	if saving != nil {
		<-saving
	}
}
