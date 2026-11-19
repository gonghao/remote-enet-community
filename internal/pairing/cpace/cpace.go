// Package cpace implements the direct pairing suite pinned to CPace draft -21.
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
	ReasonUnavailable  Reason = "unavailable"
	ReasonExpired      Reason = "expired"
	ReasonExhausted    Reason = "exhausted"
	ReasonRateLimited  Reason = "rate_limited"
	ReasonBusy         Reason = "busy"
	ReasonConfirmation Reason = "confirmation"
	ReasonStoreFailed  Reason = "store_failed"
)

func (r Reason) Error() string { return string(r) }

var errCode = errors.New("expected eight ASCII digits")
var errBinding = errors.New("invalid CPace binding")
var errState = errors.New("invalid CPace handshake state")

// Initiator handles share a single confirmation attempt, even when copied.
type Initiator struct{ s *initiatorState }
type initiatorState struct {
	mu       sync.Mutex
	scalar   *ristretto255.Scalar
	binding  Binding
	a        [32]byte
	expected *[32]byte
}

// Responder handles share a single verification attempt, even when copied.
type Responder struct{ s *responderState }
type responderState struct {
	mu      sync.Mutex
	pending *tags
}

// Start requires eight ASCII digits and a nonzero exporter and peer fingerprints.
func Start(code string, b Binding) (*Initiator, [32]byte, error) {
	s, a, err := share(code, b)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return &Initiator{&initiatorState{scalar: s, binding: b, a: a}}, a, nil
}

// Respond binds the operator's share to the client's canonical nonidentity point.
func Respond(code string, b Binding, a [32]byte) (*Responder, [32]byte, error) {
	s, y, err := share(code, b)
	if err != nil {
		return nil, [32]byte{}, err
	}
	defer s.Zero()
	shared, err := sharedPoint(s, a[:])
	if err != nil {
		return nil, [32]byte{}, err
	}
	t := confirmationTags(b, shared, a, y)
	return &Responder{&responderState{pending: &t}}, y, nil
}

// Confirm consumes the private scalar, including on invalid input.
func (i *Initiator) Confirm(y [32]byte) ([32]byte, error) {
	s := i.s
	if s == nil {
		return [32]byte{}, errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.scalar == nil {
		s.expected = nil
		return [32]byte{}, errState
	}
	shared, err := sharedPoint(s.scalar, y[:])
	s.scalar.Zero()
	s.scalar = nil
	if err != nil {
		return [32]byte{}, err
	}
	t := confirmationTags(s.binding, shared, s.a, y)
	s.expected = &t.operator
	return t.client, nil
}

// Verify consumes the expected operator tag on success or failure.
func (i *Initiator) Verify(tag [32]byte) error {
	s := i.s
	if s == nil {
		return errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.expected == nil {
		if s.scalar != nil {
			s.scalar.Zero()
			s.scalar = nil
		}
		return errState
	}
	ok := hmac.Equal(s.expected[:], tag[:])
	s.expected = nil
	if !ok {
		return ReasonConfirmation
	}
	return nil
}

// VerifyAndConfirm releases the operator tag only on successful verification.
// The caller must commit trust before sending the tag.
func (r *Responder) VerifyAndConfirm(tag [32]byte) ([32]byte, error) {
	s := r.s
	if s == nil {
		return [32]byte{}, errState
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.pending
	s.pending = nil
	if t == nil {
		return [32]byte{}, errState
	}
	if !hmac.Equal(t.client[:], tag[:]) {
		return [32]byte{}, ReasonConfirmation
	}
	return t.operator, nil
}
