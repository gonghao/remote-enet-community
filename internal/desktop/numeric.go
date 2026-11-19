package desktop

import (
	"context"
	"errors"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/invite"
	"remote-enet/internal/locales"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/settings"
)

type numericAction uint8

const (
	issueNumeric numericAction = iota
	copyNumeric
	revokeNumeric
)

var errDirectAddressRequired = errors.New("Enter the direct address and port")

// IssueNumericCode returns the secret only to the caller that requested it.
func (c *Controller) IssueNumericCode(ctx context.Context) (pairing.Display, error) {
	return c.numericOperation(ctx, issueNumeric, 0)
}

// CopyNumericCode returns the current generation's explicit clipboard text.
func (c *Controller) CopyNumericCode(ctx context.Context, generation uint64) (string, error) {
	display, err := c.numericOperation(ctx, copyNumeric, generation)
	if err != nil {
		return "", err
	}
	return display.Address + " " + display.Code[:4] + " " + display.Code[4:], nil
}

// RevokeNumericCode cannot revoke a replacement generation.
func (c *Controller) RevokeNumericCode(ctx context.Context, generation uint64) error {
	_, err := c.numericOperation(ctx, revokeNumeric, generation)
	return err
}

func (c *Controller) numericOperation(ctx context.Context, action numericAction, generation uint64) (pairing.Display, error) {
	c.mu.Lock()
	g := c.run
	ctx, finish, epoch, err := c.operationLocked(ctx, false)
	c.mu.Unlock()
	if err != nil {
		return pairing.Display{}, err
	}
	defer finish()
	if err := c.acquire(ctx); err != nil {
		return pairing.Display{}, err
	}
	defer c.release()
	c.mu.Lock()
	if epoch != c.stopEpoch || g != c.run {
		c.mu.Unlock()
		return pairing.Display{}, context.Canceled
	}
	err = c.availableLocked(ctx, false)
	if err == nil {
		switch {
		case c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator):
			err = ErrRole
		case g == nil || g.stopping || g.ctx.Err() != nil:
			err = cpace.ReasonUnavailable
		case g.shareAddress == "":
			err = invite.ErrInvalidHost
		case action == issueNumeric && g.runtime.Snapshot().Phase != app.PhaseListening:
			err = cpace.ReasonBusy
		case action != issueNumeric && (generation == 0 || generation != g.codeGeneration):
			err = cpace.ReasonUnavailable
		}
	}
	if err != nil {
		c.failureLocked(numericMessage(err), err)
		c.mu.Unlock()
		return pairing.Display{}, err
	}
	runtimeGeneration := g.runtimeCodeGeneration
	runCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	g.workers.Add(1)
	defer g.workers.Done()
	c.mu.Unlock()

	var display pairing.Display
	switch action {
	case issueNumeric:
		display, err = g.runtime.IssueNumericCode(runCtx)
	case copyNumeric:
		display, err = g.runtime.NumericCodeDisplay(runCtx, runtimeGeneration)
	case revokeNumeric:
		err = g.runtime.RevokeNumericCode(runCtx, runtimeGeneration)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || epoch != c.stopEpoch || c.run != g || g.stopping || g.ctx.Err() != nil {
		return pairing.Display{}, context.Canceled
	}
	c.refreshNumericCodeLocked()
	if runCtx.Err() != nil {
		err = runCtx.Err()
	}
	if err == nil && action != revokeNumeric {
		status := c.state.NumericCode
		if display.Generation != g.runtimeCodeGeneration || display.Address != g.shareAddress || len(display.Code) != 8 ||
			status.Committing || status.Reason != "" || !time.Now().Before(status.ExpiresAt) {
			err = cpace.ReasonUnavailable
		}
		display.Status = status
	}
	if err != nil {
		c.failureLocked(numericMessage(err), err)
		return pairing.Display{}, err
	}
	c.state.Error, c.state.PairingHint = "", ""
	c.publishLocked()
	return display, nil
}

func (c *Controller) refreshNumericCodeLocked() {
	g := c.run
	if g == nil || g.stopping || g.ctx.Err() != nil || g.shareAddress == "" {
		c.state.NumericCode = pairing.Status{}
		return
	}
	status := g.runtime.NumericCodeStatus()
	// Runtime generations restart with each listener. Desktop generations
	// remain unique across Stop, restart and replacement of the share address.
	if status.Generation != 0 {
		if status.Generation != g.runtimeCodeGeneration {
			c.codeGeneration++
			g.codeGeneration = c.codeGeneration
			g.runtimeCodeGeneration = status.Generation
		}
		status.Generation = g.codeGeneration
	}
	c.state.NumericCode = status
}

func numericMessage(err error) string {
	var reason cpace.Reason
	switch {
	case errors.As(err, &reason):
		if message := locales.PairingMessage(string(reason)); message != "" {
			return message
		}
	case errors.Is(err, pairing.ErrAlreadyRedeeming):
		return "Completing pairing"
	case errors.Is(err, pairing.ErrRedeemed):
		return "Code unavailable"
	case errors.Is(err, ErrRole):
		return "Save the selected role before starting."
	case errors.Is(err, invite.ErrInvalidHost):
		return "Invalid host"
	}
	return "Pairing interrupted"
}

func numericClientHint(kind app.PairingErrorKind) string {
	switch kind {
	case "busy", "dns":
		return "You can retry with the same code if it is still valid."
	case "unsupported", "identity_mismatch":
		return "Check the operator address before trying again."
	case "rate_limited":
		return "Try again later with a new numeric code."
	default:
		return "Request a new numeric code."
	}
}
