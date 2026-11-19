package desktop

import (
	"context"
	"errors"
	"fmt"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/settings"
)

type issuingInvite struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// IssueInvite shares the operator trust directory with the active listener.
// Hub bearer invitations are returned only to the issuing caller.
func (c *Controller) IssueInvite(ctx context.Context) (string, error) {
	ctx, finish, epoch, err := c.operation(ctx, false)
	if err != nil {
		return "", err
	}
	defer finish()
	if err := c.acquire(ctx); err != nil {
		return "", err
	}
	defer c.release()
	c.mu.Lock()
	if epoch != c.stopEpoch {
		c.mu.Unlock()
		return "", context.Canceled
	}
	c.state.Invitation = ""
	c.publishLocked()
	err = c.availableLocked(ctx, false)
	if err == nil && (c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator)) {
		err = ErrRole
	}
	g := c.run
	if err == nil && g != nil && g.stopping {
		err = ErrBusy
	}
	if err == nil && c.state.Config.Operator.Mode != "hub" && c.state.Config.Operator.Endpoint == "" {
		err = fmt.Errorf("desktop: operator endpoint required before issuing invitations")
	}
	if err != nil {
		c.failureLocked("Invitation could not be issued. Check the saved operator endpoint.", err)
		c.mu.Unlock()
		return "", err
	}
	cfg := app.InviteConfig{StateDir: c.roleDir(settings.RoleOperator), Endpoint: c.state.Config.Operator.Endpoint, TTL: 15 * time.Minute}
	if op := c.state.Config.Operator; op.Mode == "hub" {
		fp, err := identity.ParseFingerprint(op.Hub.FP)
		if err != nil {
			c.mu.Unlock()
			return "", err
		}
		cfg.Hub = &app.HubConfig{Addr: op.Hub.Addr, FP: fp}
	}
	inviteCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pending := &issuingInvite{cancel: cancel, done: make(chan struct{})}
	c.invite = pending
	defer func() {
		c.mu.Lock()
		c.invite = nil
		close(pending.done)
		c.mu.Unlock()
	}()
	ctx = inviteCtx
	if g != nil {
		g.workers.Add(1)
		defer g.workers.Done()
		stop := context.AfterFunc(g.ctx, cancel)
		defer stop()
	}
	c.mu.Unlock()
	code, err := c.deps.issueInvite(ctx, cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || epoch != c.stopEpoch || g != nil && g.stopping {
		return "", context.Canceled
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	if err != nil {
		c.failureLocked("Invitation could not be issued.", err)
		return "", fmt.Errorf("desktop: issue invitation: %w", err)
	}
	c.state.Invitation = code
	c.successLocked("Invitation issued. Expires in 15 minutes.")
	return code, nil
}

type testingLink struct {
	generation uint64
	cancel     context.CancelFunc
}

// TestLink blocks until the bounded peer-only test finishes. It borrows exactly
// the currently displayed app generation and never follows a reconnection.
func (c *Controller) TestLink(ctx context.Context) error {
	c.mu.Lock()
	if err := c.availableLocked(ctx, false); err != nil {
		c.mu.Unlock()
		return err
	}
	g := c.run
	var err error
	switch {
	case g == nil:
		err = app.ErrNotConnected
	case g.stopping || g.test != nil:
		err = linktest.ErrBusy
	case !c.state.Config.AllowLinkTest:
		err = linktest.ErrUnsupported
	case c.state.Runtime.Phase != app.PhaseConnected || c.state.Runtime.Generation == 0:
		err = app.ErrNotConnected
	}
	if err != nil {
		c.failureLocked(testMessage(err), err)
		c.mu.Unlock()
		return err
	}
	testCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	test := &testingLink{generation: c.state.Runtime.Generation, cancel: cancel}
	g.test = test
	g.workers.Add(1)
	defer g.workers.Done()
	c.state.Testing, c.state.TestResult = true, linktest.Result{}
	c.successLocked("Testing peer connection.")
	c.mu.Unlock()
	result, err := g.runtime.TestLinkForGeneration(testCtx, test.generation, linktest.Options{})
	c.mu.Lock()
	defer c.mu.Unlock()
	if testCtx.Err() != nil {
		err = testCtx.Err()
	}
	result.TimedOut = errors.Is(err, context.DeadlineExceeded)
	result.Interrupted = err != nil && !result.TimedOut
	if c.run != g || g.test != test {
		return err
	}
	g.test = nil
	c.state.Testing = false
	latest := g.runtime.Snapshot()
	if g.stopping || g.ctx.Err() != nil || c.state.Runtime.Generation != test.generation ||
		c.state.Runtime.Phase != app.PhaseConnected || latest.Generation != test.generation || latest.Phase != app.PhaseConnected {
		// Partial measurements belong to the departed generation, too.
		c.state.TestResult = linktest.Result{}
		if !g.stopping && g.ctx.Err() == nil {
			c.successLocked("Connection test canceled: connection changed.")
		} else {
			c.publishLocked()
		}
		if err == nil {
			err = app.ErrNotConnected
		}
		return err
	}
	c.state.TestResult = result
	if err != nil {
		c.failureLocked(testMessage(err), err)
		return err
	}
	c.successLocked("Connection test completed (peer-only echo).")
	return nil
}

func testMessage(err error) string {
	switch {
	case errors.Is(err, linktest.ErrUnsupported):
		return "Connection testing is disabled or unsupported by the peer."
	case errors.Is(err, linktest.ErrBusy), errors.Is(err, ErrBusy):
		return "Connection test busy."
	case errors.Is(err, context.DeadlineExceeded):
		return "Connection test timed out."
	case errors.Is(err, context.Canceled):
		return "Connection test canceled."
	case errors.Is(err, app.ErrNotConnected):
		return "Connect to a peer before testing."
	default:
		return "Connection test failed."
	}
}
