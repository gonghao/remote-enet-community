package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
)

// DigitalResponder admits one attempt after the complete commitment arrives.
type DigitalResponder interface {
	Begin(context.Context) (DigitalAttempt, error)
}

// DigitalAttempt owns the code and serializes trust persistence with revocation.
type DigitalAttempt interface {
	Context() context.Context
	Respond(cpace.Binding, [64]byte) (*cpace.Responder, [32]byte, error)
	Finish()
	// Commit reports errors from save as cpace.ReasonStoreFailed.
	Commit(func(context.Context) error) error
}

// PairingOutcome is valid only on a session returned by a successful Run.
type PairingOutcome uint8

const (
	Legacy PairingOutcome = iota
	DigitalPaired
)

// PairingOutcome reports which handshake authorized this connection.
func (s *Session) PairingOutcome() PairingOutcome { return s.outcome }

// These local reasons are never accepted as authenticated facts from the peer.
const (
	ReasonUnsupported cpace.Reason = "unsupported"
	ReasonInterrupted cpace.Reason = "interrupted"
	ReasonHubTicket   cpace.Reason = "hub_ticket"
)

func pairingFailure(r cpace.Reason) error { return fmt.Errorf("%w: %w", ErrPairing, r) }

func admissionReason(err error) (cpace.Reason, bool) {
	var r cpace.Reason
	if errors.As(err, &r) {
		switch r {
		case cpace.ReasonUnavailable, cpace.ReasonExpired, cpace.ReasonBudgetExhausted, cpace.ReasonRateLimited, cpace.ReasonBusy:
			return r, true
		}
	}
	return "", false
}

func (o Options) pairCapability() string {
	if o.Center != nil {
		return proto.PairCenterCapability
	}
	if o.HubFP.IsZero() {
		return proto.PairDirectCapability
	}
	return proto.PairHubCapability
}

func (s *Session) digital(ctx context.Context, local, peer [32]byte) error {
	exp, ok := s.ts.(interface{ PairingExporter() [32]byte })
	if !ok || s.opts.LocalFP == (identity.Fingerprint{}) || s.Peer() == (identity.Fingerprint{}) {
		return pairingFailure(ReasonInterrupted)
	}
	b := cpace.Binding{Capability: s.opts.pairCapability(), HubFP: s.opts.HubFP, Exporter: exp.PairingExporter(), ClientFP: s.opts.LocalFP, OperatorFP: s.Peer(), HelloClientHash: local, HelloOperatorHash: peer}
	if center := s.opts.Center; center != nil {
		b.HubFP, b.CenterID, b.SessionID = center.Hub, center.CenterID, center.Session
	}
	if s.opts.Role == RoleClient {
		return s.digitalClient(ctx, b)
	}
	b.ClientFP, b.OperatorFP = b.OperatorFP, b.ClientFP
	b.HelloClientHash, b.HelloOperatorHash = peer, local
	return s.digitalOperator(ctx, b)
}

func readPair[T any](r io.Reader) (T, error) {
	var zero T
	msg, err := proto.ReadPair(r)
	if err != nil {
		// Either peer can silently abort after validating a point or confirmation.
		switch any(zero).(type) {
		case proto.PakeShareA, proto.PakeConfirmB, proto.PakeConfirmA:
			return zero, pairingFailure(cpace.ReasonConfirmation)
		}
		return zero, pairingFailure(ReasonInterrupted)
	}
	if _, waiting := any(zero).(proto.PakeShareB); waiting {
		if d, ok := msg.(proto.PairDecision); ok && !d.Accepted {
			if reason, ok := admissionReason(cpace.Reason(d.Reason)); ok {
				return zero, pairingFailure(reason)
			}
		}
	}
	value, ok := msg.(T)
	if !ok {
		return zero, fmt.Errorf("%w: unexpected pairing message %T", pairingFailure(ReasonInterrupted), msg)
	}
	return value, nil
}

func (s *Session) sendPair(msg any) error {
	if err := proto.Write(s.control, msg); err != nil {
		// A write can fail after the peer consumed the frame and closed, so use the reason for the expected reply.
		// Frames 12–14 await an opening or MAC; after 15 the client is waiting for the trust decision.
		switch msg.(type) {
		case proto.PakeShareA, proto.PakeShareB, proto.PakeConfirmB:
			return pairingFailure(cpace.ReasonConfirmation)
		}
		return pairingFailure(ReasonInterrupted)
	}
	return nil
}

func (s *Session) rejectPair(reason cpace.Reason) error {
	if err := s.sendPair(proto.PairDecision{Reason: string(reason)}); err == nil {
		s.finishRejection()
	}
	return pairingFailure(reason)
}

func (s *Session) confirmSAS(ctx context.Context, sas string) error {
	if s.opts.ConfirmSAS != nil {
		// Both peers may still be comparing the SAS when the local hook returns.
		if !s.setupTimer.Stop() {
			return pairingFailure(ReasonInterrupted)
		}
		s.setupTimer.Reset(3 * time.Minute)
		confirmCtx, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(s.ctx, cancel)
		defer stop()
		defer cancel()
		ok, err := s.opts.ConfirmSAS(confirmCtx, sas, s.Peer())
		if err != nil || !ok {
			return pairingFailure(cpace.ReasonConfirmation)
		}
	}
	return nil
}

func (s *Session) digitalClient(ctx context.Context, b cpace.Binding) error {
	client, c, err := cpace.Start(s.opts.DigitalCode, b)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if s.opts.Center != nil && s.opts.BeginClientAttempt != nil && !s.opts.BeginClientAttempt() {
		return pairingFailure(cpace.ReasonBudgetExhausted)
	}
	if err = s.sendPair(proto.PakeCommit{Value: c}); err != nil {
		return err
	}
	y, err := readPair[proto.PakeShareB](s.control)
	if err != nil {
		if r, ok := admissionReason(err); s.opts.Center != nil && ok && r != cpace.ReasonBudgetExhausted {
			return pairingFailure(ReasonInterrupted)
		}
		return err
	}
	a, err := client.Reveal(y.Value)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if err = s.sendPair(proto.PakeShareA{Value: a}); err != nil {
		return err
	}
	tb, err := readPair[proto.PakeConfirmB](s.control)
	if err != nil {
		return err
	}
	ta, sas, err := client.Finish(tb.Value)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if err = s.sendPair(proto.PakeConfirmA{Value: ta}); err != nil {
		return err
	}
	if s.opts.Center != nil {
		return s.confirmCenterClient(ctx, sas)
	}
	if err = s.confirmSAS(ctx, sas); err != nil {
		return err
	}
	return s.readDecision()
}

func (s *Session) readDecision() error {
	decision, err := readPair[proto.PairDecision](s.control)
	if err != nil {
		return err
	}
	if decision.Accepted && decision.Reason == "accepted" {
		return nil
	}
	if s.opts.Center != nil && !decision.Accepted {
		switch r := cpace.Reason(decision.Reason); r {
		case cpace.ReasonRejected, cpace.ReasonConfirmTimeout:
			return pairingFailure(r)
		}
	}
	if !decision.Accepted && decision.Reason == string(ReasonHubTicket) && slices.Contains(s.NegotiatedCapabilities, proto.HubTicketCapability) {
		return pairingFailure(ReasonHubTicket)
	}
	if !decision.Accepted && decision.Reason == string(cpace.ReasonStoreFailed) {
		return pairingFailure(cpace.ReasonStoreFailed)
	}
	return pairingFailure(ReasonInterrupted)
}

func (s *Session) digitalOperator(ctx context.Context, b cpace.Binding) error {
	c, err := readPair[proto.PakeCommit](s.control)
	if err != nil {
		return err
	}
	if s.opts.DigitalManager == nil {
		return pairingFailure(ReasonInterrupted)
	}
	attempt, err := s.opts.DigitalManager.Begin(ctx)
	if err != nil {
		return s.admissionFailure(err)
	}
	defer attempt.Finish()
	operator, y, err := attempt.Respond(b, c.Value)
	if err != nil {
		return s.admissionFailure(err)
	}
	stop := context.AfterFunc(attempt.Context(), func() { s.finish(pairingFailure(ReasonInterrupted)) })
	defer stop()
	if err = s.sendPair(proto.PakeShareB{Value: y}); err != nil {
		return err
	}
	a, err := readPair[proto.PakeShareA](s.control)
	if err != nil {
		return err
	}
	tb, err := operator.Open(a.Value)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if err = s.sendPair(proto.PakeConfirmB{Value: tb}); err != nil {
		return err
	}
	ta, err := readPair[proto.PakeConfirmA](s.control)
	if err != nil {
		return err
	}
	sas, err := operator.Verify(ta.Value)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if err := s.confirmOperator(attempt.Context(), sas); err != nil {
		return err
	}
	if err := s.prepareHubTicket(); err != nil {
		return s.rejectPair(ReasonHubTicket)
	}
	// Commit rechecks the generation and may cancel the attempt context itself.
	stop()
	select {
	case <-s.ts.Done():
		return pairingFailure(ReasonInterrupted)
	default:
	}
	err = attempt.Commit(func(saveCtx context.Context) error {
		if s.opts.Remember == nil {
			return cpace.ReasonStoreFailed
		}
		return s.opts.Remember(saveCtx, b.ClientFP)
	})
	if err != nil {
		if errors.Is(err, cpace.ReasonStoreFailed) {
			return s.rejectPair(cpace.ReasonStoreFailed)
		}
		return pairingFailure(ReasonInterrupted)
	}
	return s.sendPair(proto.PairDecision{Accepted: true, Reason: "accepted"})
}

func (s *Session) admissionFailure(err error) error {
	if r, ok := admissionReason(err); ok {
		if s.opts.Center != nil && r != cpace.ReasonBudgetExhausted {
			return pairingFailure(ReasonInterrupted)
		}
		return s.rejectPair(r)
	}
	if errors.Is(err, cpace.ReasonConfirmation) {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	return pairingFailure(ReasonInterrupted)
}

func (s *Session) confirmOperator(ctx context.Context, sas string) error {
	if s.opts.Center == nil {
		return s.confirmSAS(ctx, sas)
	}
	if !s.extendCenterSetup() {
		return pairingFailure(ReasonInterrupted)
	}
	confirmCtx, cancel := context.WithCancel(ctx)
	stopConfirm := context.AfterFunc(s.ctx, cancel)
	err := s.confirmCenter(confirmCtx, sas)
	s.setupTimer.Reset(handshakeTimeout)
	stopConfirm()
	cancel()
	switch {
	case errors.Is(err, cpace.ReasonRejected):
		return s.rejectPair(cpace.ReasonRejected)
	case errors.Is(err, cpace.ReasonConfirmTimeout):
		return s.rejectPair(cpace.ReasonConfirmTimeout)
	default:
		return err
	}
}

// Leave time for the peer's manual check and for delivery of its decision.
func (s *Session) extendCenterSetup() bool {
	if !s.setupTimer.Stop() {
		return false
	}
	s.setupTimer.Reset(s.opts.ConfirmTimeout + handshakeTimeout)
	return true
}

func (s *Session) confirmCenter(ctx context.Context, sas string) error {
	prompt, cancel := context.WithTimeoutCause(ctx, s.opts.ConfirmTimeout, cpace.ReasonConfirmTimeout)
	defer cancel()
	ok, err := s.opts.ConfirmSAS(prompt, sas, s.Peer())
	if cause := context.Cause(prompt); cause != nil {
		if errors.Is(cause, cpace.ReasonConfirmTimeout) {
			return pairingFailure(cpace.ReasonConfirmTimeout)
		}
		return cause
	}
	if err != nil {
		return pairingFailure(ReasonInterrupted)
	}
	if !ok {
		return pairingFailure(cpace.ReasonRejected)
	}
	return nil
}

func (s *Session) confirmCenterClient(ctx context.Context, sas string) error {
	if !s.extendCenterSetup() {
		return pairingFailure(ReasonInterrupted)
	}
	prompt, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	decision := make(chan error, 1)
	go func() {
		err := s.readDecision()
		if err != nil {
			cancel(err)
		}
		decision <- err
	}()
	if err := s.confirmCenter(prompt, sas); err != nil {
		return err
	}
	select {
	case err := <-decision:
		return err
	case <-ctx.Done():
		return pairingFailure(ReasonInterrupted)
	}
}
