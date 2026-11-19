package session

import (
	"context"
	"errors"
	"fmt"
	"io"

	"remote-enet/internal/identity"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
)

// DigitalResponder admits one attempt after the complete client share arrives.
type DigitalResponder interface {
	Begin(context.Context) (DigitalAttempt, error)
}

// DigitalAttempt owns the code and serializes trust persistence with revocation.
type DigitalAttempt interface {
	Context() context.Context
	Respond(cpace.Binding, [32]byte) (*cpace.Responder, [32]byte, error)
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
)

func pairingFailure(r cpace.Reason) error { return fmt.Errorf("%w: %w", ErrPairing, r) }

func wireReason(err error, fallback cpace.Reason) cpace.Reason {
	var r cpace.Reason
	if errors.As(err, &r) {
		switch r {
		case cpace.ReasonUnavailable, cpace.ReasonExpired, cpace.ReasonExhausted, cpace.ReasonRateLimited, cpace.ReasonBusy, cpace.ReasonConfirmation, cpace.ReasonStoreFailed:
			return r
		}
	}
	return fallback
}

func (s *Session) digital(ctx context.Context, local, peer [32]byte) error {
	exp, ok := s.ts.(interface{ PairingExporter() [32]byte })
	if !ok || s.opts.LocalFP == (identity.Fingerprint{}) || s.Peer() == (identity.Fingerprint{}) {
		return pairingFailure(ReasonInterrupted)
	}
	b := cpace.Binding{Exporter: exp.PairingExporter(), ClientFP: s.opts.LocalFP, OperatorFP: s.Peer(), HelloClientHash: local, HelloOperatorHash: peer}
	if s.opts.Role == RoleClient {
		return s.digitalClient(b)
	}
	b.ClientFP, b.OperatorFP = b.OperatorFP, b.ClientFP
	b.HelloClientHash, b.HelloOperatorHash = peer, local
	return s.digitalOperator(ctx, b)
}

func readPair[T any](r io.Reader) (T, error) {
	var zero T
	msg, err := proto.ReadPair(r)
	if err != nil {
		return zero, pairingFailure(ReasonInterrupted)
	}
	if result, ok := msg.(proto.PairResult); ok && !result.Accepted {
		return zero, pairingFailure(wireReason(cpace.Reason(result.Reason), ReasonInterrupted))
	}
	value, ok := msg.(T)
	if !ok {
		return zero, fmt.Errorf("%w: unexpected pairing message %T", pairingFailure(ReasonInterrupted), msg)
	}
	return value, nil
}

func (s *Session) sendPair(msg any) error {
	if err := proto.Write(s.control, msg); err != nil {
		return pairingFailure(ReasonInterrupted)
	}
	return nil
}

func (s *Session) rejectPair(reason cpace.Reason) error {
	_ = s.sendPair(proto.PairResult{Reason: string(reason)})
	return pairingFailure(reason)
}

func (s *Session) digitalClient(b cpace.Binding) error {
	client, a, err := cpace.Start(s.opts.DigitalCode, b)
	if err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	if err = s.sendPair(proto.PairShareA{Value: a}); err != nil {
		return err
	}
	y, err := readPair[proto.PairShareB](s.control)
	if err != nil {
		return err
	}
	ta, err := client.Confirm(y.Value)
	if err != nil {
		return s.rejectPair(cpace.ReasonConfirmation)
	}
	if err = s.sendPair(proto.PairConfirmA{Value: ta}); err != nil {
		return err
	}
	tb, err := readPair[proto.PairConfirmB](s.control)
	if err != nil {
		return err
	}
	if err = client.Verify(tb.Value); err != nil {
		return pairingFailure(cpace.ReasonConfirmation)
	}
	return nil
}

func (s *Session) digitalOperator(ctx context.Context, b cpace.Binding) error {
	a, err := readPair[proto.PairShareA](s.control)
	if err != nil {
		return err
	}
	attempt, err := s.opts.DigitalManager.Begin(ctx)
	if err != nil {
		return s.rejectPair(wireReason(err, cpace.ReasonUnavailable))
	}
	defer attempt.Finish()
	operator, y, err := attempt.Respond(b, a.Value)
	if err != nil {
		return s.rejectPair(wireReason(err, cpace.ReasonConfirmation))
	}
	stop := context.AfterFunc(attempt.Context(), func() { s.finish(pairingFailure(ReasonInterrupted)) })
	defer stop()
	if err = s.sendPair(proto.PairShareB{Value: y}); err != nil {
		return err
	}
	ta, err := readPair[proto.PairConfirmA](s.control)
	if err != nil {
		return err
	}
	tb, err := operator.VerifyAndConfirm(ta.Value)
	if err != nil {
		return s.rejectPair(cpace.ReasonConfirmation)
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
		return s.rejectPair(wireReason(err, ReasonInterrupted))
	}
	return s.sendPair(proto.PairConfirmB{Value: tb})
}
