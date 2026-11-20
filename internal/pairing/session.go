package pairing

import (
	"context"
	"sync"
	"time"

	"remote-enet/internal/pairing/cpace"
)

// SessionResponder keeps one center session's budget across transport reconnects.
type SessionResponder struct {
	mu     sync.Mutex
	code   string
	left   int
	active *SessionAttempt
	saving chan struct{}
	closed bool
}

// NewSessionResponder takes a normalized center code and the session's remaining budget.
func NewSessionResponder(code string, budget int) *SessionResponder {
	return &SessionResponder{code: code, left: max(0, budget)}
}

// AttemptsLeft includes no refund for interrupted or successful attempts.
func (r *SessionResponder) AttemptsLeft() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.left
}

// Begin charges a complete commitment before any responder share can be generated.
func (r *SessionResponder) Begin(ctx context.Context) (*SessionAttempt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if r.closed {
		return nil, context.Canceled
	}
	if r.active != nil {
		return nil, context.Canceled
	}
	if r.left == 0 {
		return nil, cpace.ReasonBudgetExhausted
	}
	r.left--
	ctx, cancel := context.WithCancel(ctx)
	a := &SessionAttempt{r: r, ctx: ctx, cancel: cancel}
	r.active = a
	return a, nil
}

// Close invalidates uncommitted attempts and waits for any authorized pin write.
func (r *SessionResponder) Close() {
	r.mu.Lock()
	r.closed = true
	r.code = ""
	if r.active != nil {
		r.active.cancel()
	}
	saving := r.saving
	r.mu.Unlock()
	if saving != nil {
		<-saving
	}
}

// SessionAttempt is valid for a single exchange. Its caller must defer Finish.
type SessionAttempt struct {
	r         *SessionResponder
	ctx       context.Context
	cancel    context.CancelFunc
	responded bool
}

// Context ends on cancellation, Close or Finish.
func (a *SessionAttempt) Context() context.Context { return a.ctx }

// Respond consumes this attempt's right to generate a share.
func (a *SessionAttempt) Respond(b cpace.Binding, commit [64]byte) (*cpace.Responder, [32]byte, error) {
	r := a.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.ctx.Err() != nil || a.responded || r.active != a {
		return nil, [32]byte{}, context.Canceled
	}
	resp, share, err := cpace.Respond(r.code, b, commit)
	if err != nil {
		return nil, [32]byte{}, cpace.ReasonConfirmation
	}
	a.responded = true
	return resp, share, nil
}

// Finish releases the slot unless a pin write currently owns it.
func (a *SessionAttempt) Finish() {
	r := a.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == a && r.saving == nil {
		a.cancel()
		r.active = nil
	}
}

// Commit persists the pin after the caller has verified Ta and confirmed SAS.
func (a *SessionAttempt) Commit(save func(context.Context) error) error {
	r := a.r
	r.mu.Lock()
	if r.active != a || r.saving != nil || !a.responded || a.ctx.Err() != nil {
		r.mu.Unlock()
		return context.Canceled
	}
	r.saving = make(chan struct{})
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.WithoutCancel(a.ctx), 3*time.Second)
	err := save(ctx)
	cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	a.cancel()
	r.active = nil
	close(r.saving)
	r.saving = nil
	if err != nil {
		return cpace.ReasonStoreFailed
	}
	return nil
}
