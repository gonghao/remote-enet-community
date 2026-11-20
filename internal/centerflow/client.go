package centerflow

import (
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/centercode"
	"remote-enet/internal/session"
)

// ErrChooseSession requires an explicit choice before recovery can continue.
const ErrChooseSession Reason = "choose_session"

// SessionCandidate carries the ID for Resume. Only ShortID belongs in displays.
type SessionCandidate struct {
	ID      center.SessionID
	ShortID string
	// The client session object provides redemption time, not creation time.
	RedeemedAt time.Time
}

// ChooseSessionError is immutable and its message omits session identifiers.
type ChooseSessionError struct{ candidates []SessionCandidate }

func (e *ChooseSessionError) Error() string                  { return ErrChooseSession.Error() }
func (e *ChooseSessionError) Unwrap() error                  { return ErrChooseSession }
func (e *ChooseSessionError) Candidates() []SessionCandidate { return slices.Clone(e.candidates) }

func sessionChoice(live []center.Session) *ChooseSessionError {
	e := &ChooseSessionError{}
	for _, s := range live {
		e.candidates = append(e.candidates, SessionCandidate{ID: s.SessionID, ShortID: s.SessionID.String()[:6], RedeemedAt: timestamp(s.RedeemedAt)})
	}
	return e
}

// Client owns at most one in-memory PAKE budget.
type Client struct {
	*flow
	checkNIC  func(context.Context) error
	ambiguous [32]byte
}

// NewClient starts idle. Resume reconciles stored pins after restart.
func NewClient(d ClientDeps) *Client {
	return &Client{flow: newFlow(d.Dependencies, session.RoleClient), checkNIC: d.CheckNIC}
}

// Redeem verifies the local adapter before spending the single-use code.
func (c *Client) Redeem(ctx context.Context, code string) error {
	if err := c.lockCommand(ctx); err != nil {
		return err
	}
	defer c.unlockCommand()
	if c.occupied() {
		return DeviceBusy
	}
	if c.API == nil || c.CenterID == "" {
		return c.fail(ctx, CenterUnconfigured)
	}
	if c.checkNIC == nil {
		return c.fail(ctx, NICMissing)
	}
	if err := c.checkNIC(ctx); err != nil {
		return c.fail(ctx, errors.Join(NICMissing, err))
	}
	code, err := centercode.NormalizeNumericCode(code)
	if err != nil {
		return c.fail(ctx, errors.Join(CodeInvalid, err))
	}
	c.phase("redeeming", "")
	s, err := c.API.Redeem(ctx, center.NewIdempotencyKey(), code)
	digest := sha256.Sum256([]byte(code))
	retryingLostReply := ReasonOf(err) == CodeInvalid && c.ambiguous == digest
	if retryingLostReply || (errors.Is(err, center.ErrUnreachable) && ctx.Err() == nil) {
		// There is no code or redemption operation ID in the recovery list.
		// A unique live session permits retrying the user's code through PAKE.
		live, listErr := c.API.ClientSessions(ctx)
		if listErr != nil && retryingLostReply {
			return c.fail(ctx, listErr)
		}
		if len(live) > 1 {
			c.ambiguous = digest
			c.phase("idle", ErrChooseSession)
			return sessionChoice(live)
		}
		if retryingLostReply && len(live) == 1 {
			if live[0].State == "active" {
				c.ambiguous = [32]byte{}
				return c.recover(ctx, live[0])
			}
			s = live[0]
			err = nil
		}
	}
	c.ambiguous = [32]byte{}
	if err != nil {
		if errors.Is(err, center.ErrUnreachable) || ctx.Err() != nil {
			c.ambiguous = digest
		}
		c.phase("idle", ReasonOf(err))
		return c.fail(ctx, err)
	}
	c.install(s, code)
	return nil
}

// Resume selects a live session after process restart. Supply its ID when several are listed.
func (c *Client) Resume(ctx context.Context, ids ...center.SessionID) error {
	if err := c.lockCommand(ctx); err != nil {
		return err
	}
	defer c.unlockCommand()
	if c.occupied() {
		return DeviceBusy
	}
	return c.resume(ctx, ids...)
}
func (c *Client) resume(ctx context.Context, ids ...center.SessionID) error {
	if err := c.prune(ctx); err != nil {
		return c.fail(ctx, err)
	}
	if len(ids) == 1 {
		s, err := c.API.Session(ctx, ids[0], 0, 0, false)
		if ReasonOf(err) == NotFound {
			return errors.Join(c.fail(ctx, err), c.Store.DeletePin(ctx, ids[0]))
		}
		if err != nil {
			return c.fail(ctx, err)
		}
		return c.recover(ctx, s)
	}
	live, err := c.API.ClientSessions(ctx)
	if err != nil {
		return c.fail(ctx, err)
	}
	if len(ids) > 1 {
		return c.fail(ctx, sessionChoice(live))
	}
	switch len(live) {
	case 0:
		return nil
	case 1:
		return c.recover(ctx, live[0])
	default:
		return c.fail(ctx, sessionChoice(live))
	}
}

// Run serves released data connections until cancellation or a terminal result.
func (c *Client) Run(ctx context.Context, base session.Options, serve func(context.Context, *session.Session) error) error {
	return c.run(ctx, base, serve, func(ctx context.Context) error { return c.resume(ctx) })
}

// Retry authorizes one fresh PAKE attempt after an interrupted exchange.
func (c *Client) Retry(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.current == nil || c.current.retry == nil {
		return errors.New("centerflow: no pairing attempt awaiting retry")
	}
	close(c.current.retry)
	c.current.retry = nil
	return nil
}
