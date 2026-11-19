package app

import (
	"context"
	"errors"
	"net"

	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	hubtransport "remote-enet/internal/transport/hub"
)

type digitalResponder struct {
	*pairing.Manager
	listener context.Context
}

func (d digitalResponder) Begin(ctx context.Context) (session.DigitalAttempt, error) {
	attempt, err := d.Manager.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return attempt, nil
}

// PairingErrorKind carries only a localizable reason, never credentials or peer text.
type PairingErrorKind string

func (k PairingErrorKind) Error() string { return string(k) }

func classifyPairingError(err error) PairingErrorKind {
	var reason cpace.Reason
	var dns *net.DNSError
	var timeout net.Error
	switch {
	case errors.As(err, &dns):
		return "dns"
	case errors.As(err, &timeout) && timeout.Timeout():
		return "timeout"
	case errors.Is(err, identity.ErrFingerprintMismatch):
		return "identity_mismatch"
	case errors.Is(err, hubtransport.ErrPeerBusy):
		return "busy"
	case errors.As(err, &reason):
		switch reason {
		case cpace.ReasonUnavailable, cpace.ReasonExpired, cpace.ReasonExhausted, cpace.ReasonRateLimited, cpace.ReasonBusy, cpace.ReasonConfirmation, cpace.ReasonStoreFailed, session.ReasonUnsupported:
			return PairingErrorKind(reason)
		}
	}
	return "interrupted"
}

func (r *Runtime) numericAvailableLocked() bool {
	return r.numeric != nil && r.numeric.listener.Err() == nil && !r.stopped
}

// IssueNumericCode replaces a generation only while the listener can accept a client.
func (r *Runtime) IssueNumericCode(ctx context.Context) (pairing.Display, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return pairing.Display{}, err
	}
	if !r.numericAvailableLocked() || r.numeric.Status().Address == "" {
		return pairing.Display{}, cpace.ReasonUnavailable
	}
	if r.state.Phase != PhaseListening {
		return pairing.Display{}, cpace.ReasonBusy
	}
	return r.numeric.Issue()
}

// NumericCodeStatus is safe for general UI state.
func (r *Runtime) NumericCodeStatus() pairing.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.numericAvailableLocked() {
		return pairing.Status{Reason: cpace.ReasonUnavailable}
	}
	return r.numeric.Status()
}

// NumericCodeDisplay releases the secret only for the selected live generation.
func (r *Runtime) NumericCodeDisplay(ctx context.Context, generation uint64) (pairing.Display, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return pairing.Display{}, err
	}
	if !r.numericAvailableLocked() {
		return pairing.Display{}, cpace.ReasonUnavailable
	}
	return r.numeric.Display(generation)
}

// RevokeNumericCode cannot revoke an irreversible trust commit.
func (r *Runtime) RevokeNumericCode(ctx context.Context, generation uint64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.numericAvailableLocked() {
		return cpace.ReasonUnavailable
	}
	return r.numeric.Revoke(ctx, generation)
}
