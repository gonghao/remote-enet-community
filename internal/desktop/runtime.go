package desktop

import (
	"context"
	"fmt"
	"net/netip"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/checkup"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/linktest"
	"remote-enet/internal/operator"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

type running struct {
	runtime      appRuntime
	ctx          context.Context
	cancel       context.CancelFunc
	done         chan struct{}
	workers      sync.WaitGroup
	stopping     bool
	test         *testingLink
	direct       *directOperation
	capture      *hsfzcapture.Recorder
	captureStart *captureOperation
	captureStop  *captureOperation
	peerAttempt  identity.Fingerprint
	pairing      transport.PeerInfo
}

// StartClient accepts an asynchronous start using the saved configuration.
// Startup failures are published in State; ctx only bounds admission.
func (c *Controller) StartClient(ctx context.Context, invitation string) error {
	return c.start(ctx, settings.RoleClient, strings.TrimSpace(strings.TrimPrefix(invitation, "\uFEFF")))
}

// StartOperator stops listening only when Stop, Close or the parent context ends.
func (c *Controller) StartOperator(ctx context.Context) error {
	return c.start(ctx, settings.RoleOperator, "")
}

func (c *Controller) start(ctx context.Context, role, invitation string) error {
	ctx, finish, epoch, err := c.operation(ctx, true)
	if err != nil {
		c.failure("Runtime could not be started.", err)
		return err
	}
	defer finish()
	c.mu.Lock()
	if c.check != nil {
		c.check.cancel()
	}
	c.mu.Unlock()
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	c.mu.Lock()
	defer c.mu.Unlock()
	if epoch != c.stopEpoch {
		return context.Canceled
	}
	if err := c.availableLocked(ctx, true); err != nil {
		c.failureLocked("Runtime could not be started.", err)
		return err
	}
	cfg := c.state.Config
	if cfg.Role != role || !c.allowsRole(role) {
		c.failureLocked("Save the selected role before starting.", ErrRole)
		return ErrRole
	}
	client, op, pairing, err := c.runtimeConfigs(cfg, invitation)
	if err != nil {
		c.failureLocked("Check the saved settings and invitation before starting.", err)
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(c.ctx)
	g := &running{runtime: c.deps.newRuntime(), ctx: runCtx, cancel: cancel, done: make(chan struct{}), pairing: pairing}
	if role == settings.RoleOperator {
		g.capture = hsfzcapture.New(hsfzcapture.Options{})
		op.Capture = g.capture
	}
	c.run = g
	c.state.Checkup = checkup.Report{}
	c.state.Capture = hsfzcapture.Snapshot{}
	c.state.Runtime = app.Snapshot{Role: session.Role(role), Phase: app.PhaseStarting}
	c.state.Running, c.state.Stopping, c.state.Testing = true, false, false
	c.state.DirectWorking = false
	c.state.Invitation, c.state.Error = "", ""
	c.state.TestResult = linktest.Result{}
	c.successLocked("Runtime starting.")
	go c.serve(g, role, client, op)
	return nil
}

func (c *Controller) runtimeConfigs(cfg settings.Config, code string) (app.ClientConfig, app.OperatorConfig, transport.PeerInfo, error) {
	var pairing transport.PeerInfo
	client := app.ClientConfig{StateDir: c.roleDir(settings.RoleClient), NICName: cfg.Client.NICName,
		AllowLinkTest: cfg.AllowLinkTest, Logger: quietLogger()}
	op := app.OperatorConfig{StateDir: c.roleDir(settings.RoleOperator), Listen: cfg.Operator.Listen,
		AllowLinkTest: cfg.AllowLinkTest, Exclusive: cfg.Operator.Exclusive, Logger: quietLogger(),
		DirectFactory: c.opts.DirectFactory,
		Ports:         operator.Ports{Diagnostic: cfg.Operator.DiagnosticPort, Control: cfg.Operator.ControlPort, Discovery: cfg.Operator.DiscoveryPort}}
	if err := cfg.Validate(); err != nil {
		return client, op, pairing, fmt.Errorf("desktop: invalid settings: %w", err)
	}
	if cfg.Role == settings.RoleClient {
		if cfg.Client.NICAddr != "" {
			client.NICAddr, _ = netip.ParseAddr(cfg.Client.NICAddr)
		}
		if code != "" {
			i, err := invite.Decode(code)
			if err != nil || i.Peer.ID.IsZero() || i.Token == ([16]byte{}) || len(i.Peer.Endpoints) == 0 {
				return client, op, pairing, fmt.Errorf("desktop: invalid invitation")
			}
			client.Invite = code
			pairing = i.Peer
		} else if cfg.Client.Peer != "" {
			client.Peer, _ = identity.ParseFingerprint(cfg.Client.Peer)
		} else {
			return client, op, pairing, fmt.Errorf("desktop: invitation or saved paired peer required")
		}
	} else {
		if cfg.Operator.Endpoint == "" || cfg.Operator.Bind == "" {
			return client, op, pairing, fmt.Errorf("desktop: operator endpoint and presentation IP required")
		}
		if cfg.Operator.Exclusive && runtime.GOOS != "windows" {
			return client, op, pairing, fmt.Errorf("desktop: exclusive mode requires Windows: %w", exclusive.ErrUnsupported)
		}
		op.Bind, _ = netip.ParseAddr(cfg.Operator.Bind)
	}
	return client, op, pairing, nil
}

func (c *Controller) serve(g *running, role string, client app.ClientConfig, op app.OperatorConfig) {
	// Subscribe before running so a fast startup failure cannot be missed.
	states := g.runtime.Subscribe(g.ctx)
	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		var captureTick <-chan time.Time
		if g.capture != nil {
			timer := time.NewTicker(time.Second / 4)
			defer timer.Stop()
			captureTick = timer.C
		}
		for {
			select {
			case <-g.ctx.Done():
				return
			case <-captureTick:
				c.mu.Lock()
				if c.run == g && !g.stopping {
					latest := g.capture.Snapshot()
					if !reflect.DeepEqual(c.state.Capture, latest) {
						failed := c.state.Capture.Error == "" && latest.Error != ""
						c.state.Capture = latest
						if failed {
							c.failureLocked("HSFZ capture stopped with an error; the file may be incomplete.", fmt.Errorf("capture: %s", latest.Error))
						} else {
							c.publishLocked()
						}
					}
				}
				c.mu.Unlock()
			case s, ok := <-states:
				if !ok {
					return
				}
				c.acceptSnapshot(g, s)
			}
		}
	}()
	var err error
	if role == settings.RoleClient {
		err = g.runtime.RunClient(g.ctx, client)
	} else {
		err = g.runtime.RunOperator(g.ctx, op)
	}
	c.mu.Lock()
	wasStopping := g.stopping || c.ctx.Err() != nil
	g.stopping = true
	g.cancel()
	if c.run == g {
		c.state.Stopping = true
		c.publishLocked()
	}
	c.mu.Unlock()
	<-pumpDone
	g.workers.Wait()
	if g.capture != nil {
		_ = g.capture.Stop(context.Background())
	}
	final := displayRuntime(g.runtime.Snapshot())
	c.recoverPairing(g, final)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run == g {
		c.state.Runtime = final
		c.state.Runtime.Phase = app.PhaseStopped
		c.state.Running, c.state.Stopping, c.state.Testing = false, false, false
		c.state.DirectWorking = false
		c.state.TestResult = linktest.Result{}
		c.state.Invitation = ""
		c.refreshCaptureLocked()
		c.run = nil
		if err != nil && !wasStopping {
			c.failureLocked("Runtime stopped with an error. Check runtime diagnostics.", err)
		} else if c.state.Capture.Error != "" {
			c.failureLocked("Runtime stopped. HSFZ capture ended with an error; the file may be incomplete.", fmt.Errorf("capture: %s", c.state.Capture.Error))
		} else {
			c.successLocked("Runtime stopped.")
		}
	}
	close(g.done)
}

func (c *Controller) acceptSnapshot(g *running, s app.Snapshot) {
	s = displayRuntime(s)
	c.mu.Lock()
	if c.run != g || g.stopping || g.ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	previous := c.state.Runtime
	if s.Phase != app.PhaseIdle {
		c.state.Runtime = s
	}
	if previous.Generation != s.Generation || s.Phase != app.PhaseConnected {
		if g.test != nil {
			g.test.cancel()
		}
		c.state.TestResult = linktest.Result{}
		if g.direct != nil {
			g.direct.cancel()
		}
	}
	if c.check != nil && c.check.run == g && !sameCheckRuntime(c.check.sample, s) {
		c.check.cancel()
	}
	if !sameCheckRuntime(previous, s) {
		c.state.Checkup = checkup.Report{}
	}
	if previous.Phase != s.Phase {
		if message := phaseEvent(s.Phase); message != "" {
			c.eventLocked(message)
		}
	}
	remember := s.Role == session.RoleClient && s.Phase == app.PhaseConnected && s.Generation != 0 &&
		!s.Peer.IsZero() && s.Peer != g.peerAttempt && c.state.Config.Client.Peer != s.Peer.String()
	c.publishLocked()
	c.mu.Unlock()
	if remember {
		c.rememberPeer(g.ctx, g, s.Peer, false)
	}
}

func (c *Controller) rememberPeer(ctx context.Context, g *running, peer identity.Fingerprint, finishing bool) {
	if c.acquire(ctx) != nil {
		return
	}
	defer c.release()
	c.mu.Lock()
	if c.run != g || !finishing && (g.stopping || g.ctx.Err() != nil) || ctx.Err() != nil {
		c.mu.Unlock()
		return
	}
	g.peerAttempt = peer
	cfg := c.state.Config
	cfg.Client.Peer = peer.String()
	c.mu.Unlock()
	err := c.deps.save(c.opts.ConfigPath, cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failureLocked("Paired peer could not be saved. Use an invitation next time.", err)
		return
	}
	c.state.Config = cfg
	if c.check != nil {
		c.check.cancel()
	}
	c.state.Checkup = checkup.Report{}
	c.successLocked("Paired peer saved.")
}

func (c *Controller) recoverPairing(g *running, final app.Snapshot) {
	// app subscriptions intentionally coalesce. A successful, very short
	// session can disappear before connected is read. Generation alone is not
	// proof of persistence: app allocates it before SavePeer, so verify the
	// exact invited identity AND endpoints against the durable trust record.
	c.mu.Lock()
	needed := !g.pairing.ID.IsZero() && final.Generation != 0 && final.Peer == g.pairing.ID &&
		g.peerAttempt != g.pairing.ID && c.state.Config.Client.Peer != g.pairing.ID.String()
	c.mu.Unlock()
	if !needed {
		return
	}
	// Cleanup owns a bounded budget independently of the canceled run. Stop
	// and Close still join it before releasing the run reservation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), time.Second)
	defer cancel()
	if err := c.deps.pairedPeer(ctx, c.roleDir(settings.RoleClient), g.pairing); err != nil {
		return
	}
	c.rememberPeer(ctx, g, g.pairing.ID, true)
}

// Stop cancels the current run even if ctx has expired. Returning nil means
// all run workers have exited; a canceled wait leaves the run reserved.
func (c *Controller) Stop(ctx context.Context) error {
	c.mu.Lock()
	g := c.run
	invitation := c.invite
	check := c.check
	c.stopLocked()
	c.mu.Unlock()
	if g != nil {
		if err := wait(ctx, g.done); err != nil {
			return err
		}
	}
	if invitation != nil {
		if err := wait(ctx, invitation.done); err != nil {
			return err
		}
	}
	if check != nil {
		return wait(ctx, check.done)
	}
	return nil
}

func (c *Controller) stopLocked() {
	// Invalidate admitted requests still waiting for configuration I/O. Stop
	// must not wait for unrelated atomic saves to release the gate.
	c.stopEpoch++
	c.state.Checkup = checkup.Report{}
	if c.check != nil {
		c.check.cancel()
	}
	changed := c.state.Invitation != ""
	c.state.Invitation = ""
	if c.invite != nil {
		c.invite.cancel()
	}
	if g := c.run; g != nil && !g.stopping {
		g.stopping = true
		g.cancel()
		c.state.Stopping = true
		c.state.TestResult = linktest.Result{}
		c.state.Notice, c.state.Error = "Stopping runtime.", ""
		c.eventLocked("Stopping runtime.")
		c.publishLocked()
	} else if c.run == nil {
		c.successLocked("Runtime stopped.")
	} else if changed {
		c.publishLocked()
	}
}
