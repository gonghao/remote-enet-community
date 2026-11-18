package desktop

import (
	"context"
	"fmt"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

type captureOperation struct {
	cancel context.CancelFunc
	done   chan struct{}
	err    error
}

// MarkCapture is bounded and performs no file I/O. It may run directly in a
// UI or hotkey callback so scheduling other actions cannot delay its timestamp.
func (c *Controller) MarkCapture(ctx context.Context, label string) (hsfzcapture.Marker, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.availableLocked(ctx, false); err != nil {
		return hsfzcapture.Marker{}, err
	}
	if c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator) {
		return hsfzcapture.Marker{}, ErrRole
	}
	g := c.run
	if g == nil || g.stopping || g.captureStop != nil {
		return hsfzcapture.Marker{}, hsfzcapture.ErrNotRecording
	}
	marker, err := g.capture.Mark(label, g.runtime.Snapshot().Generation)
	if err == nil {
		c.publishLocked()
	}
	return marker, err
}

func (c *Controller) refreshCaptureLocked() {
	if g := c.run; g != nil && g.capture != nil {
		c.state.Capture = g.capture.Snapshot()
	}
}

// StartCapture records only newly admitted streams in the current Operator run.
// The request context bounds setup, not the lifetime of a successful recording.
func (c *Controller) StartCapture(ctx context.Context, path string) error {
	c.mu.Lock()
	err := c.availableLocked(ctx, false)
	g := c.run
	if err == nil {
		switch {
		case c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator):
			err = ErrRole
		case g == nil:
			err = fmt.Errorf("desktop: start the Operator before capturing")
		case g.stopping || g.captureStart != nil || g.captureStop != nil:
			err = ErrBusy
		default:
			s := g.capture.Snapshot()
			if s.Active || s.Stopping {
				err = ErrBusy
			}
		}
	}
	if err != nil {
		c.failureLocked("HSFZ capture could not start. Start the Operator and choose a new local file.", err)
		c.mu.Unlock()
		return err
	}
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(g.ctx, cancel)
	op := &captureOperation{cancel: cancel, done: make(chan struct{})}
	g.captureStart = op
	g.workers.Add(1)
	c.mu.Unlock()
	defer g.workers.Done()
	defer close(op.done)
	defer func() { stop(); cancel() }()
	err = g.capture.Start(opCtx, path)
	if opCtx.Err() != nil {
		// File creation cannot be interrupted mid-syscall. Roll back a late
		// success before releasing this run's worker reservation.
		_ = g.capture.Stop(context.Background())
		err = opCtx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g.captureStart = nil
	if c.run != g || g.stopping {
		c.publishLocked()
		return err
	}
	if s := g.capture.Snapshot(); err == nil && s.Error != "" {
		err = fmt.Errorf("capture: %s", s.Error)
	}
	if err != nil {
		c.failureLocked("HSFZ capture could not start. Choose a new file in a writable local folder.", err)
	} else {
		c.successLocked("HSFZ capture started. Reconnect existing diagnostic tools to capture their TCP streams.")
	}
	return err
}

// StopCapture starts cleanup even if its caller abandons the wait. It also
// cancels and joins an admitted StartCapture before closing that recording.
func (c *Controller) StopCapture(ctx context.Context) error {
	c.mu.Lock()
	if c.closed || c.ctx.Err() != nil {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.state.Config.Role != settings.RoleOperator || !c.allowsRole(settings.RoleOperator) {
		c.mu.Unlock()
		return ErrRole
	}
	g := c.run
	if g == nil {
		c.mu.Unlock()
		return nil
	}
	if g.stopping {
		c.mu.Unlock()
		return wait(ctx, g.done)
	}
	op := g.captureStop
	if op == nil {
		pending := g.captureStart
		if pending != nil {
			pending.cancel()
		}
		op = &captureOperation{done: make(chan struct{})}
		g.captureStop = op
		g.workers.Add(1)
		go c.stopCapture(g, op, pending)
	}
	c.mu.Unlock()
	if err := wait(ctx, op.done); err != nil {
		return err
	}
	return op.err
}

func (c *Controller) stopCapture(g *running, op, pending *captureOperation) {
	defer g.workers.Done()
	defer close(op.done)
	if pending != nil {
		<-pending.done
	}
	op.err = g.capture.Stop(context.Background())
	s := g.capture.Snapshot()
	if op.err == nil && s.Error != "" {
		op.err = fmt.Errorf("capture: %s", s.Error)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	g.captureStop = nil
	if c.run != g || g.stopping {
		c.publishLocked()
		return
	}
	if op.err != nil {
		c.failureLocked("HSFZ capture ended with an error; the file may be incomplete.", op.err)
	} else if s.Incomplete {
		c.successLocked("HSFZ capture stopped. The file contains incomplete data; review gap and partial frame records.")
	} else {
		c.successLocked("HSFZ capture stopped and the JSONL file is closed.")
	}
}
