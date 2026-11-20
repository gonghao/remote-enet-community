package centerflow

import (
	"context"
	"errors"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hub "remote-enet/internal/transport/hub"
)

type serveFunc = func(context.Context, *session.Session) error
type remoteEnd struct{ Reason }

func (f *flow) read(ctx context.Context, r *reception, since int64, wait time.Duration) (center.Session, error) {
	s, err := retryAPI(ctx, f, func() (center.Session, error) { return f.API.Session(ctx, r.remote.SessionID, since, wait, false) })
	if err == nil && s.State == "ended" {
		err = remoteEnd{Reason(*s.EndReason)}
	}
	return s, err
}

func (f *flow) run(parent context.Context, base session.Options, serve serveFunc, resume func(context.Context) error) error {
	if f.Confirm == nil || f.Connector == nil || serve == nil {
		return errors.New("centerflow: confirmation, connector and serve callback required")
	}
	if err := f.lockCommand(parent); err != nil {
		return err
	}
	f.mu.Lock()
	running := f.done != nil
	f.mu.Unlock()
	if running {
		f.unlockCommand()
		return errRunning
	}
	if f.current == nil {
		if err := resume(parent); err != nil {
			f.unlockCommand()
			return err
		}
	}
	r := f.current
	if r == nil {
		f.unlockCommand()
		return nil
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	f.mu.Lock()
	f.cancel = cancel
	f.done = done
	f.mu.Unlock()
	f.unlockCommand()
	defer func() {
		cancel()
		f.mu.Lock()
		r.retry = nil
		if f.current != nil && parent.Err() != nil {
			f.state.Phase = "paused"
		}
		f.cancel = nil
		f.done = nil
		close(done)
		f.mu.Unlock()
	}()
	if r.pendingEnd != "" {
		return f.end(ctx, r, r.pendingEnd)
	}
	work, stop := context.WithCancelCause(ctx)
	ready := make(chan center.Session, 1)
	active := make(chan struct{})
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		var version int64
		var wait time.Duration
		sent, activated := false, false
		for {
			s, err := f.read(work, r, version, wait)
			if err != nil {
				stop(err)
				return
			}
			version = s.Version
			wait = 25 * time.Second
			f.mu.Lock()
			f.state.ExpiresAt = timestamp(s.ExpiresAt)
			f.mu.Unlock()
			if s.State != "open" && !sent {
				sent = true
				f.mu.Lock()
				if f.role == session.RoleOperator {
					r.code = ""
				}
				f.state.CodeExpiresAt = time.Time{}
				f.state.PeerShort = f.peer(s).Short()
				f.state.Phase = "authorized"
				f.mu.Unlock()
				ready <- s
			}
			if s.State == "active" && !activated {
				activated = true
				close(active)
			}
		}
	}()
	var err error
	select {
	case s := <-ready:
		err = f.connections(work, r, s, active, base, serve)
	case <-work.Done():
	}
	stop(err)
	<-watched
	if ctx.Err() != nil {
		return ctx.Err()
	}
	err = context.Cause(work)
	var ended remoteEnd
	if errors.As(err, &ended) {
		return f.cleanup(ctx, r, ended.Reason)
	}
	if why := pairEnd(err); why != "" {
		return f.end(ctx, r, why)
	}
	return f.handle(ctx, r, err)
}

func pairEnd(err error) Reason {
	switch {
	case errors.Is(err, cpace.ReasonNotPaired), errors.Is(err, cpace.ReasonStoreFailed), errors.Is(err, PairingLost):
		return PairingLost
	case errors.Is(err, cpace.ReasonBudgetExhausted):
		return "pake_failed"
	case errors.Is(err, cpace.ReasonRejected):
		return "rejected"
	case errors.Is(err, cpace.ReasonConfirmTimeout):
		return "confirm_timeout"
	}
	return ""
}

func (f *flow) connect(ctx context.Context, r *reception, s center.Session) (transport.Session, error) {
	var t center.Ticket
	var rejected bool
	for delay := f.RetryMin; ; delay = min(2*delay, f.RetryMax) {
		if t.Raw == nil {
			key := center.NewIdempotencyKey()
			var err error
			t, err = retryAPI(ctx, f, func() (center.Ticket, error) { return f.API.Ticket(ctx, key, s.SessionID) })
			if ReasonOf(err) == "session_not_ready" {
				state, readErr := f.read(ctx, r, 0, 0)
				for readErr == nil && state.State == "open" {
					state, readErr = f.read(ctx, r, state.Version, 25*time.Second)
				}
				if readErr != nil {
					return nil, readErr
				}
				if err := sleep(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			if err != nil {
				return nil, err
			}
		}
		// Ticket verifies against a fresh API read. Also bind it to this
		// reception, whose peer may already have a locally persisted pin.
		if t.PeerFP != f.peer(s) || t.Node.NodeID != s.Node.NodeID || t.Node.HubFP != s.Node.HubFP {
			return nil, PairingLost
		}
		ep := transport.Endpoint{Kind: "hub", Addr: addr(t.Node), Hub: t.Node.HubFP}
		connect := f.Connector.WaitTicket
		if f.role == session.RoleClient {
			connect = f.Connector.DialTicket
		}
		ts, err := connect(ctx, ep, t.PeerFP, t.Raw)
		if err == nil {
			return ts, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		switch {
		case errors.Is(err, hub.ErrUnauthorized):
			for {
				if _, err = f.read(ctx, r, 0, 0); err != nil {
					return nil, err
				}
				if !rejected {
					break
				}
				f.phase("waiting", HubUnauthorized)
				if err = sleep(ctx, delay); err != nil {
					return nil, err
				}
				delay = min(delay*2, f.RetryMax)
			}
			rejected = true
			t = center.Ticket{}
			continue
		case errors.Is(err, hub.ErrPeerBusy):
			f.phase("waiting", Busy)
		case errors.Is(err, hub.ErrUnknownPeer):
			f.phase("waiting", OperatorWaiting)
		case errors.Is(err, identity.ErrFingerprintMismatch):
			return nil, err
		default:
			t = center.Ticket{}
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
		if f.role == session.RoleOperator || time.Until(time.Unix(t.TicketExpiresAt, 0)) < 10*time.Second {
			t = center.Ticket{}
		}
	}
}

type responder struct{ *pairing.SessionResponder }

func (r responder) Begin(ctx context.Context) (session.DigitalAttempt, error) {
	return r.SessionResponder.Begin(ctx)
}

func (f *flow) connections(ctx context.Context, r *reception, s center.Session, active <-chan struct{}, base session.Options, serve serveFunc) error {
	base.Role = f.role
	base.LocalFP = f.LocalFP
	base.Initiator = f.role == session.RoleClient
	base.HoldTunnels = true
	base.Center = &session.CenterBinding{CenterID: f.CenterID, Hub: s.Node.HubFP, Session: s.SessionID}
	base.DigitalManager = nil
	if r.responder != nil {
		base.DigitalManager = responder{r.responder}
	}
	base.ConfirmSAS = func(ctx context.Context, code string, peer identity.Fingerprint) (bool, error) {
		if r.verified == peer {
			return true, nil
		}
		f.phase("confirming", "")
		deadline, _ := ctx.Deadline()
		ok, err := f.Confirm(ctx, SASPrompt{promptID.Add(1), code, peer, deadline})
		if ok && err == nil && ctx.Err() == nil {
			r.verified = peer
		}
		return ok, err
	}
	base.BeginClientAttempt = func() bool {
		if r.left == 0 {
			return false
		}
		r.left--
		f.mu.Lock()
		f.state.AttemptsLeft = r.left
		f.mu.Unlock()
		return true
	}
	base.Pinned = func(fp identity.Fingerprint) bool {
		p, err := f.Store.Pin(s.SessionID)
		return err == nil && p.Peer == fp && f.matches(p, s)
	}
	base.Remember = func(ctx context.Context, fp identity.Fingerprint) error { return f.save(ctx, s, fp) }
	for delay := f.RetryMin; ; {
		f.phase("waiting", "")
		f.mu.Lock()
		base.DigitalCode = r.code
		f.mu.Unlock()
		if f.role == session.RoleOperator {
			base.DigitalCode = ""
		} else if base.DigitalCode == "" && !base.Pinned(s.OperatorFP) {
			return PairingLost
		}
		ts, err := f.connect(ctx, r, s)
		if err != nil {
			return err
		}
		sess, err := session.Run(ctx, ts, base)
		if r.responder != nil {
			f.mu.Lock()
			f.state.AttemptsLeft = r.responder.AttemptsLeft()
			f.mu.Unlock()
		}
		if err != nil {
			ts.Close(err)
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if pairEnd(err) != "" || errors.Is(err, session.ReasonUnsupported) || errors.Is(err, session.ErrVersion) {
				return err
			}
			if base.DigitalCode != "" {
				if r.left == 0 {
					return cpace.ReasonBudgetExhausted
				}
				again := make(chan struct{})
				f.mu.Lock()
				r.retry = again
				f.state.Phase = "retry"
				f.state.Reason = Confirmation
				f.mu.Unlock()
				select {
				case <-again:
				case <-ctx.Done():
					return ctx.Err()
				}
			} else if err := sleep(ctx, delay); err != nil {
				return err
			}
			continue
		}
		if sess.PairingOutcome() == session.DigitalPaired {
			if f.role == session.RoleOperator {
				// The client may still be in its SAS dialog. Closing the mux here can discard frame 16 in transit.
				timeout := base.ConfirmTimeout
				if timeout == 0 {
					timeout = 3 * time.Minute
				}
				timer := time.NewTimer(timeout + 10*time.Second)
				select {
				case <-sess.Done():
				case <-ctx.Done():
				case <-timer.C:
				}
				timer.Stop()
			} else {
				if err := f.save(ctx, s, sess.Peer()); err != nil {
					ts.Close(err)
					return errors.Join(PairingLost, err)
				}
				f.mu.Lock()
				r.code = ""
				f.mu.Unlock()
			}
			ts.Close(nil)
			continue
		}
		f.phase("awaiting_center", "awaiting_center")
		err = f.serveData(ctx, r, s, sess, active, serve)
		ts.Close(err)
		if err != nil {
			return err
		}
		if err = sleep(ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, f.RetryMax)
	}
}

func (f *flow) serveData(ctx context.Context, r *reception, s center.Session, ss *session.Session, active <-chan struct{}, serve serveFunc) error {
	data, cancel := context.WithCancel(ctx)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		select {
		case <-ss.Done():
			cancel()
		case <-data.Done():
		}
	}()
	defer func() { cancel(); <-joined }()
	var err error
	if f.role == session.RoleOperator {
		_, err = retryAPI(data, f, func() (center.Session, error) {
			reply, err := f.API.Established(data, r.establishedKey, s.SessionID, ss.Peer())
			if err == nil && reply.State != "active" {
				return reply, errors.New("centerflow: center did not confirm active session")
			}
			return reply, err
		})
	} else {
		select {
		case <-active:
		case <-data.Done():
		}
	}
	if err == nil && data.Err() == nil {
		ss.ReleaseTunnels()
		f.phase("active", "")
		err = serve(data, ss)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	select {
	case <-ss.Done():
		if errors.Is(ss.Err(), cpace.ReasonNotPaired) {
			return ss.Err()
		}
		return nil
	default:
	}
	return err
}
