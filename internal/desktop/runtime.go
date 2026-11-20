package desktop

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"reflect"
	"runtime"
	"slices"
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
	"remote-enet/internal/locales"
	"remote-enet/internal/operator"
	"remote-enet/internal/pairing"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

type running struct {
	runtime        appRuntime
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	workers        sync.WaitGroup
	stopping       bool
	test           *testingLink
	direct         *directOperation
	capture        *hsfzcapture.Recorder
	captureStart   *captureOperation
	captureStop    *captureOperation
	peerAttempt    identity.Fingerprint
	pairing        transport.PeerInfo
	digitalPair    bool
	peerSaveFailed bool

	shareAddress          string
	codeGeneration        uint64
	runtimeCodeGeneration uint64
}

// StartClient accepts an asynchronous start using the saved configuration.
// Startup failures are published in State; ctx only bounds admission.
func (c *Controller) StartClient(ctx context.Context, input string) error {
	// The app also decodes raw invitations, so pass it the legacy trimmed text.
	input = strings.TrimSpace(input)
	return c.start(ctx, settings.RoleClient, strings.TrimSpace(strings.TrimPrefix(input, "\uFEFF")))
}

// StartOperator stops listening only when Stop, Close or the parent context ends.
func (c *Controller) StartOperator(ctx context.Context) error {
	return c.start(ctx, settings.RoleOperator, "")
}

func (c *Controller) start(ctx context.Context, role, input string) error {
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
	client, op, peer, err := c.runtimeConfigs(cfg, input)
	if err != nil {
		message := "Check the saved settings and invitation before starting."
		switch err {
		case invite.ErrInvalidInvite, invite.ErrInviteExpired, invite.ErrInvalidHost, invite.ErrInvalidPort, invite.ErrInvalidCode, errDirectAddressRequired, errCenterCode:
			message = err.Error()
		}
		c.failureLocked(message, err)
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	runCtx, cancel := context.WithCancel(c.ctx)
	g := &running{runtime: c.deps.newRuntime(), ctx: runCtx, cancel: cancel, done: make(chan struct{}), pairing: peer}
	logger := slog.New(runtimeLogHandler{controller: c, run: g})
	client.Logger, op.Logger = logger, logger
	if client.DirectPair != nil {
		g.digitalPair = true
		address := client.DirectPair.Address
		client.Hooks.OnPaired = func(peer transport.PeerInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.run == g && !peer.ID.IsZero() && len(peer.Endpoints) == 1 &&
				pairedEndpoint(peer.Endpoints[0], peer.ID, address) {
				peer.Endpoints = slices.Clone(peer.Endpoints)
				g.pairing = peer
			}
		}
	}
	if role == settings.RoleOperator {
		g.shareAddress = op.ShareAddress
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

func pairedEndpoint(ep transport.Endpoint, peer identity.Fingerprint, address string) bool {
	if ep.Kind != "hub" || ep.Addr != address || ep.Hub.IsZero() {
		return false
	}
	if ep.Hub == peer {
		return ep.Ticket == ""
	}
	return ep.Ticket != ""
}

func (c *Controller) runtimeConfigs(cfg settings.Config, code string) (app.ClientConfig, app.OperatorConfig, transport.PeerInfo, error) {
	var pairing transport.PeerInfo
	client := app.ClientConfig{StateDir: c.roleDir(settings.RoleClient), NICName: cfg.Client.NICName,
		AllowLinkTest: cfg.AllowLinkTest}
	op := app.OperatorConfig{StateDir: c.roleDir(settings.RoleOperator), Listen: cfg.Operator.Listen,
		AllowLinkTest: cfg.AllowLinkTest, Exclusive: cfg.Operator.Exclusive,
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
			input, err := invite.ParseInput(code)
			if err != nil {
				return client, op, pairing, err
			}
			switch input.Kind {
			case invite.Raw:
				i := input.Invite
				if i.Peer.ID.IsZero() || i.Token == ([16]byte{}) || len(i.Peer.Endpoints) == 0 {
					return client, op, pairing, invite.ErrInvalidInvite
				}
				client.Invite = code
				pairing = i.Peer
			case invite.Direct:
				client.DirectPair = &app.DirectPair{Address: input.Address, Code: input.Code}
			case invite.Center:
				if cfg.Center.ID != "" {
					return client, op, pairing, errCenterCode
				}
				return client, op, pairing, errDirectAddressRequired
			}
		} else if cfg.Client.Peer != "" {
			client.Peer, _ = identity.ParseFingerprint(cfg.Client.Peer)
		} else {
			return client, op, pairing, fmt.Errorf("desktop: invitation or saved paired peer required")
		}
	} else {
		share := cfg.Operator.Endpoint
		if cfg.Operator.Mode == "hub" {
			fp, _ := identity.ParseFingerprint(cfg.Operator.Hub.FP) // cfg.Validate checked the pin.
			op.Listen, op.Hub = "", &app.HubConfig{Addr: cfg.Operator.Hub.Addr, FP: fp}
			share = cfg.Operator.Hub.Share
			if share == "" {
				share = op.Hub.Addr
			}
		}
		if share == "" || cfg.Operator.Bind == "" {
			return client, op, pairing, fmt.Errorf("desktop: operator endpoint and presentation IP required")
		}
		if cfg.Operator.Exclusive && runtime.GOOS != "windows" {
			return client, op, pairing, fmt.Errorf("desktop: exclusive mode requires Windows: %w", exclusive.ErrUnsupported)
		}
		op.Bind, _ = netip.ParseAddr(cfg.Operator.Bind)
		// Reuse direct-input address validation for sharing. A legacy endpoint
		// that cannot be used for numeric pairing still supports raw invitations.
		op.ShareAddress, _ = invite.ParseAddress(share)
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
		var lastCodeUpdate time.Time
		var lastCodeStatus pairing.Status
		if g.capture != nil {
			timer := time.NewTicker(time.Second / 4)
			defer timer.Stop()
			captureTick = timer.C
		}
		for {
			select {
			case <-g.ctx.Done():
				return
			case now := <-captureTick:
				c.mu.Lock()
				if c.run == g && !g.stopping {
					c.refreshNumericCodeLocked()
					codeChanged := c.state.NumericCode != lastCodeStatus ||
						now.Sub(lastCodeUpdate) >= time.Second && c.state.NumericCode.Reason == "" && c.state.NumericCode.Generation != 0
					latest := g.capture.Snapshot()
					captureChanged := !reflect.DeepEqual(c.state.Capture, latest)
					failed := captureChanged && c.state.Capture.Error == "" && latest.Error != ""
					if captureChanged {
						c.state.Capture = latest
					}
					if codeChanged {
						lastCodeUpdate = now
						lastCodeStatus = c.state.NumericCode
					}
					if failed {
						c.failureLocked("HSFZ capture stopped with an error; the file may be incomplete.", fmt.Errorf("capture: %s", latest.Error))
					} else if captureChanged || codeChanged {
						c.publishLocked()
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
		c.state.NumericCode = pairing.Status{}
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
		var pairingErr app.PairingErrorKind
		var pairingText string
		if errors.As(err, &pairingErr) {
			pairingText = locales.PairingMessage(string(pairingErr))
		}
		switch {
		case !wasStopping && pairingText != "":
			var hint string
			if pairingErr == app.HubAuthorizationExpired {
				hint = "Request a new numeric code."
			} else if g.digitalPair && g.pairing.ID.IsZero() {
				hint = numericClientHint(pairingErr)
			}
			c.state.Runtime.LastError = pairingText
			c.setFailureLocked(pairingText, nil, hint)
		case g.peerSaveFailed:
			c.setFailureLocked("Could not save pairing", nil, "Request a new numeric code.")
		case !wasStopping && (errors.Is(err, session.ErrPairing) || errors.Is(err, session.ErrVersion)):
			c.setFailureLocked(final.LastError, nil, "")
		case err != nil && !wasStopping:
			c.failureLocked("Runtime stopped with an error. Check runtime diagnostics.", err)
		case c.state.Capture.Error != "":
			c.failureLocked("Runtime stopped. HSFZ capture ended with an error; the file may be incomplete.", fmt.Errorf("capture: %s", c.state.Capture.Error))
		default:
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
	if s.Phase != app.PhaseIdle && s.Phase != app.PhaseStarting && c.state.Notice == "Runtime starting." {
		c.state.Notice = ""
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
	if g.digitalPair {
		remember = remember && g.pairing.ID == s.Peer
	}
	c.refreshNumericCodeLocked()
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
		if g.digitalPair {
			g.peerSaveFailed = true
			c.setFailureLocked("Could not save pairing", err, "Request a new numeric code.")
		} else {
			c.failureLocked("Paired peer could not be saved. Use an invitation next time.", err)
		}
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
	// session can disappear before connected is read. Check the durable record
	// for the exact peer and endpoints. A numeric callback can arrive before
	// the app publishes a connection generation.
	c.mu.Lock()
	peer := g.pairing
	needed := !peer.ID.IsZero() && (g.digitalPair || final.Generation != 0 && final.Peer == peer.ID) &&
		g.peerAttempt != peer.ID && c.state.Config.Client.Peer != peer.ID.String()
	c.mu.Unlock()
	if !needed {
		return
	}
	// Cleanup owns a bounded budget independently of the canceled run. Stop
	// and Close still join it before releasing the run reservation.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.ctx), time.Second)
	defer cancel()
	if err := c.deps.pairedPeer(ctx, c.roleDir(settings.RoleClient), peer); err != nil {
		return
	}
	c.rememberPeer(ctx, g, peer.ID, true)
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
	changed := c.state.Invitation != "" || c.state.NumericCode != (pairing.Status{})
	c.state.Invitation = ""
	c.state.NumericCode = pairing.Status{}
	if c.invite != nil {
		c.invite.cancel()
	}
	if g := c.run; g != nil && !g.stopping {
		g.stopping = true
		g.cancel()
		c.state.Stopping = true
		c.state.TestResult = linktest.Result{}
		c.state.Notice, c.state.Error = "Stopping runtime.", ""
		c.state.PairingHint = ""
		c.eventLocked("Stopping runtime.")
		c.publishLocked()
	} else if c.run == nil {
		c.successLocked("Runtime stopped.")
	} else if changed {
		c.publishLocked()
	}
}
