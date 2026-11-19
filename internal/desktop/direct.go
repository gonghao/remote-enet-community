package desktop

import (
	"context"
	"errors"
	"os"

	"remote-enet/internal/app"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

type directRuntime interface {
	ConnectMGU(context.Context, uint64) error
	DisconnectMGU(context.Context, uint64) error
}

type directOperation struct {
	generation uint64
	enable     bool
	cancel     context.CancelFunc
	done       chan struct{}
}

// ConnectMGU binds the setup request to the displayed run and session generation.
func (c *Controller) ConnectMGU(ctx context.Context) error {
	return c.directLink(ctx, true, false, nil, 0)
}

// ConnectEVO binds endpoint detection to the displayed session generation.
func (c *Controller) ConnectEVO(ctx context.Context) error {
	return c.directLink(ctx, true, true, nil, 0)
}

// DisconnectMGU also cancels an admitted setup before removing its resources.
func (c *Controller) DisconnectMGU(ctx context.Context) error {
	c.mu.Lock()
	if err := c.availableLocked(ctx, false); err != nil {
		c.mu.Unlock()
		return err
	}
	g, generation := c.run, c.state.Runtime.Generation
	if g == nil || c.state.Runtime.Phase != app.PhaseConnected {
		c.mu.Unlock()
		return app.ErrNotConnected
	}
	var pending *directOperation
	if g.direct != nil && g.direct.enable {
		pending = g.direct
		pending.cancel()
	}
	c.mu.Unlock()
	if pending != nil {
		if err := wait(ctx, pending.done); err != nil {
			return err
		}
	}
	return c.directLink(ctx, false, false, g, generation)
}

func (c *Controller) directLink(ctx context.Context, enable, evo bool, expected *running, generation uint64) error {
	c.mu.Lock()
	if err := c.availableLocked(ctx, false); err != nil {
		c.mu.Unlock()
		return err
	}
	g := c.run
	var err error
	switch {
	case expected != nil && (g != expected || c.state.Runtime.Generation != generation):
		err = app.ErrNotConnected
	case c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator):
		err = ErrRole
	case g == nil || c.state.Runtime.Phase != app.PhaseConnected || c.state.Runtime.Generation == 0:
		err = app.ErrNotConnected
	case g.stopping || g.direct != nil:
		err = ErrBusy
	}
	var runtime directRuntime
	var connect func(context.Context, uint64) error
	if err == nil {
		var supported bool
		runtime, supported = g.runtime.(directRuntime)
		if !supported {
			err = transport.ErrUnsupported
		} else {
			connect = runtime.ConnectMGU
			if evo {
				t, ok := g.runtime.(interface {
					ConnectEVO(context.Context, uint64) error
				})
				if ok {
					connect = t.ConnectEVO
				} else {
					err = transport.ErrUnsupported
				}
			}
		}
	}
	if err != nil {
		c.failureLocked(directMessage(err), err)
		c.mu.Unlock()
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	defer func() { stop(); cancel() }()
	op := &directOperation{generation: c.state.Runtime.Generation, enable: enable, cancel: cancel, done: make(chan struct{})}
	g.direct = op
	g.workers.Add(1)
	defer g.workers.Done()
	defer close(op.done)
	c.state.DirectWorking = true
	message := "Preparing direct link."
	if !enable {
		message = "Stopping direct link."
	}
	c.successLocked(message)
	c.mu.Unlock()
	if enable {
		err = connect(opCtx, op.generation)
	} else {
		err = runtime.DisconnectMGU(opCtx, op.generation)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if opCtx.Err() != nil {
		err = opCtx.Err()
	}
	if c.run != g || g.direct != op {
		return err
	}
	g.direct = nil
	c.state.DirectWorking = false
	latest := g.runtime.Snapshot()
	if g.stopping || g.ctx.Err() != nil || latest.Generation != op.generation || latest.Phase != app.PhaseConnected {
		c.publishLocked()
		if err == nil {
			err = app.ErrNotConnected
		}
		return err
	}
	c.state.Runtime = displayRuntime(latest)
	if err == nil && (enable && latest.Direct.Phase != app.DirectReady || !enable && latest.Direct.Phase != app.DirectDisabled) {
		c.failureLocked("Direct link canceled: vehicle connection changed.", app.ErrNotConnected)
		return app.ErrNotConnected
	}
	if err != nil {
		c.failureLocked(directMessage(err), err)
		return err
	}
	message = "Direct link ready."
	if !enable {
		message = "Direct link stopped."
	}
	c.successLocked(message)
	return nil
}

func directMessage(err error) string {
	switch {
	case errors.Is(err, ErrRole):
		return "Direct link is available in Operator."
	case errors.Is(err, app.ErrNotConnected):
		return "Connect to a vehicle before enabling direct access."
	case errors.Is(err, ErrBusy), errors.Is(err, exclusive.ErrBusy):
		return "Direct link is busy. Close active diagnostic connections and try again."
	case errors.Is(err, context.Canceled):
		return "Direct link operation canceled."
	case errors.Is(err, context.DeadlineExceeded):
		return "Direct link operation timed out."
	case errors.Is(err, transport.ErrUnsupported):
		return "Direct link is unsupported by this runtime."
	case errors.Is(err, os.ErrPermission):
		return "Direct link needs system network configuration privileges."
	default:
		return "Direct link could not start. Check vehicle support, permissions, and route conflicts."
	}
}
