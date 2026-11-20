package centerflow

import (
	"context"
	"crypto/sha256"
	"errors"

	"remote-enet/internal/center"
	"remote-enet/internal/centercode"
	"remote-enet/internal/session"
)

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
	if ReasonOf(err) == CodeInvalid && c.ambiguous == digest {
		// There is no code or redemption operation ID in the recovery list.
		// A unique live session permits retrying the user's code through PAKE.
		live, listErr := c.API.ClientSessions(ctx)
		if listErr != nil {
			return c.fail(ctx, listErr)
		}
		if len(live) == 1 {
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
	if len(ids) > 1 {
		return errors.New("centerflow: select one session")
	}
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
	switch len(live) {
	case 0:
		return nil
	case 1:
		return c.recover(ctx, live[0])
	default:
		return errors.New("centerflow: select a client session")
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
