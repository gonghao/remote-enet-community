package app

import (
	"context"
	"errors"
	"net"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	hubtransport "remote-enet/internal/transport/hub"
)

type digitalResponder struct {
	*pairing.Manager
	listener         context.Context
	refresh          func()
	grant            func(identity.Fingerprint) ([]byte, error)
	waitToken        string
	waitGeneration   uint64
	deniedGeneration uint64
}

func (d *digitalResponder) Begin(ctx context.Context) (session.DigitalAttempt, error) {
	attempt, err := d.Manager.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if d.refresh == nil {
		return attempt, nil
	}
	return &windowAttempt{Attempt: attempt, responder: d}, nil
}

// Finish runs after the handshake has either committed or released its budget.
type windowAttempt struct {
	*pairing.Attempt
	responder *digitalResponder
}

func (a *windowAttempt) Finish() {
	a.Attempt.Finish()
	if a.responder.Status().Reason != "" {
		a.responder.refresh()
	}
}

func (r *Runtime) onRegister(d *digitalResponder, token []byte, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d.listener.Err() != nil || r.stopped || string(token) != d.waitToken || r.state.Phase != PhaseListening {
		return
	}
	if errors.Is(err, context.Canceled) {
		return
	}
	r.state.HubWaiting = err == nil
	r.state.NextRetry = time.Time{}
	switch {
	case err == nil:
		r.state.LastError = ""
	case errors.Is(err, HubIssuer):
		r.state.LastError = string(HubIssuer)
	case errors.Is(err, hub.ErrUnauthorized):
		st := d.Status()
		if st.Reason == "" && st.Generation == d.waitGeneration && d.Revoke(d.listener, st.Generation) == nil {
			d.deniedGeneration = st.Generation
			r.state.LastError = string(HubPairingDisabled)
			d.refresh()
		} else {
			r.state.LastError = string(HubWaitUnauthorized)
			r.state.NextRetry = time.Now().Add(30 * time.Second)
		}
	case errors.Is(err, identity.ErrFingerprintMismatch):
		r.state.LastError = string(HubIdentityMismatch)
	case errors.Is(err, hub.ErrPeerBusy):
		r.state.LastError = "busy"
	default:
		r.state.LastError = string(HubUnreachable)
	}
	r.publishLocked()
}

// PairingErrorKind carries only a localizable reason, never credentials or peer text.
type PairingErrorKind string

const (
	HubIssuer               PairingErrorKind = "hub_issuer"
	HubIdentityMismatch     PairingErrorKind = "hub_identity_mismatch"
	HubWaitUnauthorized     PairingErrorKind = "hub_wait_unauthorized"
	HubPairingDisabled      PairingErrorKind = "hub_pairing_disabled"
	HubAuthorizationExpired PairingErrorKind = "hub_authorization_expired_or_revoked"
	HubUnreachable          PairingErrorKind = "hub_unreachable"
	OperatorOffline         PairingErrorKind = "operator_offline"
)

func (k PairingErrorKind) Error() string { return string(k) }

func classifyPairingError(err error) PairingErrorKind {
	var kind PairingErrorKind
	var reason cpace.Reason
	var dns *net.DNSError
	var timeout net.Error
	switch {
	case errors.As(err, &kind):
		return kind
	case errors.Is(err, hub.ErrUnknownPeer):
		return OperatorOffline
	case errors.Is(err, hub.ErrUnauthorized):
		return PairingErrorKind(cpace.ReasonUnavailable)
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
		case cpace.ReasonUnavailable, cpace.ReasonExpired, cpace.ReasonBudgetExhausted, cpace.ReasonRateLimited, cpace.ReasonBusy, cpace.ReasonConfirmation, cpace.ReasonStoreFailed, session.ReasonUnsupported, session.ReasonHubTicket:
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
	if r.state.Phase != PhaseListening && !(r.numeric.refresh != nil && r.state.Phase == PhaseConnected) {
		return pairing.Display{}, cpace.ReasonBusy
	}
	display, err := r.numeric.Issue()
	if err == nil && r.numeric.refresh != nil {
		r.state.HubWaiting = false
		r.publishLocked()
		r.numeric.waitToken = ""
		r.numeric.refresh()
	}
	return display, err
}

// NumericCodeStatus is safe for general UI state.
func (r *Runtime) NumericCodeStatus() pairing.Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.numericAvailableLocked() {
		return pairing.Status{Reason: cpace.ReasonUnavailable}
	}
	st := r.numeric.Status()
	if st.Generation != 0 && st.Generation == r.numeric.deniedGeneration {
		st.Reason = cpace.Reason(HubPairingDisabled)
	}
	return st
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
	err := r.numeric.Revoke(ctx, generation)
	if err == nil && r.numeric.refresh != nil {
		r.state.HubWaiting = false
		r.publishLocked()
		r.numeric.waitToken = ""
		r.numeric.refresh()
	}
	return err
}
