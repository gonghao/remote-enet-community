package desktop

import (
	"context"
	"errors"
	"slices"
	"strings"

	"remote-enet/internal/app"
	"remote-enet/internal/center"
	"remote-enet/internal/centercode"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/locales"
	"remote-enet/internal/settings"
)

// CheckCenter returns the saved center snapshot alongside its results so the
// UI can discard them if the user changes the center during the request.
func (c *Controller) CheckCenter(ctx context.Context) (settings.Center, []center.EntryResult, error) {
	c.mu.Lock()
	ctx, finish, _, err := c.operationLocked(ctx, false)
	role, cfg := c.state.Config.Role, c.state.Config.Center
	cfg.API = slices.Clone(cfg.API)
	c.mu.Unlock()
	if err != nil {
		return settings.Center{}, nil, err
	}
	defer finish()

	results := c.deps.checkCenter(ctx, cfg, role, c.opts.AppVersion)
	return cfg, results, ctx.Err()
}

// CenterView carries public account and reception metadata. Countdown controls
// use the absolute deadlines so coalesced updates do not extend a deadline.
type CenterView struct {
	app.CenterSnapshot
	LoggedIn bool
	// EffectiveExpiry is the effective authorization deadline in Unix seconds.
	EffectiveExpiry  int64
	Account          center.Account
	Devices          []center.DeviceEntry
	Nodes            []centerflow.NodeLatency
	RequiresSASInput bool
}

// ErrSASInputRequired means the live operator prompt still needs matching input.
var ErrSASInputRequired = errors.New("desktop: matching operator input required")

// ErrCenterOperation hides unrecognized server errors from desktop callers.
var ErrCenterOperation = errors.New("desktop: center operation failed")

// CenterDisplay returns secrets exclusively for live local controls. On the
// operator side MatchSAS checks the input without disclosing the expected digits.
func (c *Controller) CenterDisplay() (string, centerflow.SASPrompt) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != nil && c.run.center && !c.run.stopping {
		invitation, prompt := c.run.runtime.CenterDisplay()
		if c.state.Config.Role == settings.RoleOperator {
			prompt.Code = ""
		}
		return invitation, prompt
	}
	return "", centerflow.SASPrompt{}
}

// MatchSAS checks operator input locally and binds a positive answer to this prompt.
func (c *Controller) MatchSAS(id uint64, input string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	g := c.run
	if g == nil || g.stopping || !g.center || c.state.Config.Role != settings.RoleOperator {
		return false
	}
	_, prompt := g.runtime.CenterDisplay()
	digits := strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '　':
			return -1
		case r >= '０' && r <= '９':
			return '0' + r - '０'
		default:
			return r
		}
	}, input)
	g.matchedSAS = 0
	if id != 0 && id == prompt.ID && len(digits) == 6 && digits == prompt.Code {
		g.matchedSAS = id
		return true
	}
	return false
}

// StartReception creates a reception on an available node in the refreshed list.
func (c *Controller) StartReception(ctx context.Context, nodeID string) error {
	if nodeID == "" {
		return centerflow.NodeUnavailable
	}
	return c.start(ctx, settings.RoleOperator, "", nodeID)
}

// AnswerSAS consumes a single live prompt. A stale dialog cannot confirm a replacement.
func (c *Controller) AnswerSAS(ctx context.Context, id uint64, match bool) error {
	return c.receptionAction(ctx, func(ctx context.Context, g *running) error {
		c.mu.Lock()
		needsInput := match && c.state.Config.Role == settings.RoleOperator && (id == 0 || g.matchedSAS != id)
		c.mu.Unlock()
		if needsInput {
			_, p := g.runtime.CenterDisplay()
			if id != 0 && id == p.ID {
				return ErrSASInputRequired
			}
			return app.ErrSASExpired
		}
		return g.runtime.AnswerSAS(ctx, id, match)
	})
}

// RetryPairing releases a client waiting for a manual retry.
func (c *Controller) RetryPairing(ctx context.Context) error {
	return c.receptionAction(ctx, func(ctx context.Context, g *running) error {
		if g.runtime.Snapshot().Center.Phase != "retry" {
			return app.ErrNotConnected
		}
		return g.runtime.RetryPairing(ctx)
	})
}

// EndReception records a terminal decision at the center.
func (c *Controller) EndReception(ctx context.Context) error {
	return c.receptionAction(ctx, func(ctx context.Context, g *running) error { return g.runtime.EndReception(ctx) })
}

func (c *Controller) receptionAction(ctx context.Context, action func(context.Context, *running) error) error {
	c.mu.Lock()
	if err := c.availableLocked(ctx, false); err != nil {
		c.mu.Unlock()
		return err
	}
	g := c.run
	if g == nil || !g.center || g.stopping {
		c.mu.Unlock()
		return app.ErrNotConnected
	}
	g.workers.Add(1)
	defer g.workers.Done()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	defer func() { stop(); cancel() }()
	c.mu.Unlock()
	return publicCenterError(action(ctx, g))
}

// Do not retain arbitrary envelope codes, server details or signed request URLs.
func publicCenterError(err error) error {
	if err == nil {
		return nil
	}
	for _, known := range []error{context.Canceled, context.DeadlineExceeded, ErrBusy, ErrRole, app.ErrNotConnected, app.ErrSASExpired, ErrSASInputRequired, centercode.ErrCodeLength, centercode.ErrCodeCharacter} {
		if errors.Is(err, known) {
			return known
		}
	}
	reason := centerflow.ReasonOf(err)
	if locales.CenterMessage(string(reason), "operator") != "" {
		return reason
	}
	return ErrCenterOperation
}

func (c *Controller) centerAccount(ctx context.Context, cfg settings.Center) (*centerflow.Operator, error) {
	return c.deps.newRuntime().CenterAccount(ctx, c.roleDir(settings.RoleOperator), app.CenterConfig{Center: cfg, AppVersion: c.opts.AppVersion})
}

func (c *Controller) accountAction(ctx context.Context, action func(context.Context, *centerflow.Operator, *CenterView) error) error {
	ctx, finish, _, err := c.operation(ctx, true)
	if err != nil {
		return err
	}
	defer finish()
	if err = c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	c.mu.Lock()
	err = c.availableLocked(ctx, true)
	cfg, view := c.state.Config, c.state.Center
	c.mu.Unlock()
	if err != nil {
		return err
	}
	if cfg.Role != settings.RoleOperator || cfg.Operator.Mode != "center" {
		return ErrRole
	}
	op, err := c.centerAccount(ctx, cfg.Center)
	if err == nil {
		err = action(ctx, op, &view)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if centerflow.ReasonOf(err) == centerflow.Unauthenticated {
		view = CenterView{}
	}
	c.state.Center = view
	if err != nil {
		key := locales.CenterMessage(string(centerflow.ReasonOf(err)), cfg.Role)
		if errors.Is(err, centercode.ErrCodeLength) {
			key = "Account code must be 8 characters"
		}
		if key == "" {
			key = "Action failed"
		}
		c.failureLocked(key, err)
	} else {
		c.state.Error = ""
		c.publishLocked()
	}
	return publicCenterError(err)
}

func refreshAccount(ctx context.Context, op *centerflow.Operator, v *CenterView) error {
	account, err := op.Account(ctx)
	if err != nil {
		return err
	}
	v.LoggedIn, v.Account, v.EffectiveExpiry = true, account.Account, account.ExpiresAt
	devices, err := op.API.Devices(ctx)
	if err != nil {
		return err
	}
	v.Devices = devices
	nodes, err := op.Nodes(ctx)
	if err == nil {
		v.Nodes = nodes
	}
	return err
}

// CenterLogin enrolls the local operator identity and reads its account.
func (c *Controller) CenterLogin(ctx context.Context, accountCode, label string) error {
	return c.accountAction(ctx, func(ctx context.Context, op *centerflow.Operator, v *CenterView) error {
		if v.LoggedIn {
			return ErrBusy
		}
		if err := op.Login(ctx, accountCode, label); err != nil {
			return err
		}
		v.LoggedIn = true
		return refreshAccount(ctx, op, v)
	})
}

// RefreshCenter obtains authoritative occupancy and measures node TCP latency.
func (c *Controller) RefreshCenter(ctx context.Context) error {
	return c.accountAction(ctx, refreshAccount)
}

// CenterLogout revokes the local signer and clears its cached enrollment.
func (c *Controller) CenterLogout(ctx context.Context) error {
	return c.accountAction(ctx, func(ctx context.Context, op *centerflow.Operator, v *CenterView) error {
		if !v.LoggedIn {
			return centerflow.Unauthenticated
		}
		if err := op.Logout(ctx); err != nil {
			return err
		}
		*v = CenterView{}
		return nil
	})
}

// RevokeCenterDevice accepts only a device from the displayed account.
func (c *Controller) RevokeCenterDevice(ctx context.Context, fp string) error {
	return c.accountAction(ctx, func(ctx context.Context, op *centerflow.Operator, v *CenterView) error {
		if !v.LoggedIn {
			return centerflow.Unauthenticated
		}
		for _, d := range v.Devices {
			if d.DeviceFP.String() != fp {
				continue
			}
			if d.Current {
				if err := op.Logout(ctx); err != nil {
					return err
				}
				*v = CenterView{}
				return nil
			}
			if err := op.API.RevokeDevice(ctx, center.NewIdempotencyKey(), d.DeviceFP); err != nil {
				return err
			}
			return refreshAccount(ctx, op, v)
		}
		return centerflow.NotFound
	})
}
