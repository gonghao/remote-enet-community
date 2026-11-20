// Package cpace implements the numeric pairing suite pinned to CPace draft -21.
package cpace

import (
	"crypto/hmac"
	"errors"
	"sync"

	"github.com/gtank/ristretto255"
)

// Reason values are protocol identifiers shared by the pairing manager, session and UI.
type Reason string

const (
	ReasonUnavailable     Reason = "unavailable"
	ReasonExpired         Reason = "expired"
	ReasonBudgetExhausted Reason = "budget_exhausted"
	ReasonRateLimited     Reason = "rate_limited"
	ReasonBusy            Reason = "busy"
	ReasonConfirmation    Reason = "confirmation"
	ReasonRejected        Reason = "rejected"
	ReasonConfirmTimeout  Reason = "confirm_timeout"
	ReasonNotPaired       Reason = "not_paired"
	ReasonStoreFailed     Reason = "store_failed"
)

func (r Reason) Error() string { return string(r) }

var errCode = errors.New("expected eight ASCII digits")
var errBinding = errors.New("invalid CPace binding")
var errState = errors.New("invalid CPace handshake state")

// Initiator handles share one commitment and confirmation attempt, even when copied.
type Initiator struct{ s *exchange }

// Responder handles share one opening and verification attempt, even when copied.
type Responder struct{ s *exchange }
type exchange struct {
	mu      sync.Mutex
	scalar  *ristretto255.Scalar
	binding Binding
	own     [32]byte
	commit  [64]byte
	pending *tags
}

// Start hides the initiator share until the responder chooses its own.
func Start(code string, b Binding) (*Initiator, [64]byte, error) {
	scalar, a, err := share(code, b)
	if err != nil {
		return nil, [64]byte{}, err
	}
	return &Initiator{&exchange{scalar: scalar, binding: b, own: a}}, commitment(a), nil
}

// Respond generates a fresh share for each commitment.
func Respond(code string, b Binding, commit [64]byte) (*Responder, [32]byte, error) {
	scalar, y, err := share(code, b)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return &Responder{&exchange{scalar: scalar, binding: b, own: y, commit: commit}}, y, nil
}

// Reveal consumes the scalar even if the peer share is invalid.
func (i *Initiator) Reveal(y [32]byte) ([32]byte, error) {
	s := i.s
	if s == nil {
		return [32]byte{}, errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.derive(y, s.own, y)
	if err != nil {
		return [32]byte{}, err
	}
	return s.own, nil
}

// Open checks the committed share before releasing Tb.
func (r *Responder) Open(a [32]byte) ([64]byte, error) {
	s := r.s
	if s == nil {
		return [64]byte{}, errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := commitment(a)
	if !hmac.Equal(c[:], s.commit[:]) {
		s.discard()
		return [64]byte{}, ReasonConfirmation
	}
	if err := s.derive(a, a, s.own); err != nil {
		return [64]byte{}, err
	}
	return s.pending.operator, nil
}

func (s *exchange) discard() {
	if s.scalar != nil {
		s.scalar.Zero()
		s.scalar = nil
	}
	s.pending = nil
}
func (s *exchange) derive(peer, a, y [32]byte) error {
	if s.scalar == nil {
		s.discard()
		return errState
	}
	shared, err := sharedPoint(s.scalar, peer[:])
	s.discard()
	if err != nil {
		return err
	}
	t := confirmationTags(s.binding, shared, a, y)
	s.pending = &t
	return nil
}

// Finish releases Ta and SAS only after verifying Tb.
func (i *Initiator) Finish(tb [64]byte) ([64]byte, string, error) {
	s := i.s
	if s == nil {
		return [64]byte{}, "", errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.pending
	s.discard()
	if t == nil {
		return [64]byte{}, "", errState
	}
	if !hmac.Equal(tb[:], t.operator[:]) {
		return [64]byte{}, "", ReasonConfirmation
	}
	return t.client, t.sas, nil
}

// Verify consumes the expected Ta on success or failure.
func (r *Responder) Verify(ta [64]byte) (string, error) {
	s := r.s
	if s == nil {
		return "", errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.pending
	s.discard()
	if t == nil {
		return "", errState
	}
	if !hmac.Equal(ta[:], t.client[:]) {
		return "", ReasonConfirmation
	}
	return t.sas, nil
}
