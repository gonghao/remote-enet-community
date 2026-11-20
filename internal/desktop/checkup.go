package desktop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/checkup"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

type checkingConnection struct {
	cancel context.CancelFunc
	done   chan struct{}
	run    *running
	sample app.Snapshot
}

// CheckConnection uses saved settings and public peer endpoints. Invitation is
// decoded in memory only; no token, identity or trust mutation is performed.
func (c *Controller) CheckConnection(ctx context.Context, invitation string) error {
	c.mu.Lock()
	if err := c.availableLocked(ctx, false); err != nil {
		c.mu.Unlock()
		return err
	}
	if c.check != nil || c.run != nil && (c.run.stopping || c.run.ctx.Err() != nil) {
		c.mu.Unlock()
		return ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	stopController := context.AfterFunc(c.ctx, cancel)
	defer stopController()
	c.ops.Add(1)
	defer c.ops.Done()
	cfg, g, epoch := c.state.Config, c.run, c.stopEpoch
	pending := &checkingConnection{cancel: cancel, done: make(chan struct{}), run: g}
	if g != nil {
		pending.sample = g.runtime.Snapshot()
		g.workers.Add(1)
		defer g.workers.Done()
		stop := context.AfterFunc(g.ctx, cancel)
		defer stop()
	}
	c.check = pending
	c.state.Checking, c.state.Checkup = true, checkup.Report{}
	c.successLocked("Checking saved connection settings…")
	c.mu.Unlock()
	defer close(pending.done)

	report := checkup.Report{StartedAt: time.Now()}
	err := ctx.Err()
	if g == nil && err == nil {
		// Idle probes hold the configuration gate until every socket has
		// closed. Start cancels and joins this work before opening listeners.
		err = c.acquire(ctx)
		if err == nil {
			defer c.release()
		}
	}
	c.mu.Lock()
	current := epoch == c.stopEpoch && cfg.Equal(c.state.Config) && c.run == g
	c.mu.Unlock()
	if err == nil && current {
		in := checkup.Input{AppVersion: c.opts.AppVersion, Config: cfg, Runtime: pending.sample, Running: g != nil}
		if cfg.Role == settings.RoleClient {
			peer, code := c.checkEndpoints(ctx, cfg, invitation)
			in.PeerID, in.Endpoints, in.PeerCode = peer.ID, slices.Clone(peer.Endpoints), code
			for i := range in.Endpoints {
				in.Endpoints[i].Ticket = ""
			}
		}
		if cfg.Role == settings.RoleOperator && cfg.Operator.Mode == "center" {
			in.AccountStatus = func(ctx context.Context) error {
				id, err := identity.LoadExisting(filepath.Join(c.roleDir(settings.RoleOperator), "identity"))
				if errors.Is(err, os.ErrNotExist) {
					return centerflow.Unauthenticated
				}
				if err != nil {
					return err
				}
				api, err := center.NewClient(center.ClientOptions{Center: cfg.Center, Role: cfg.Role, AppVersion: c.opts.AppVersion, Device: id.Key})
				if err != nil {
					return err
				}
				_, err = api.Account(ctx)
				return err
			}
		}
		if cfg.Role == settings.RoleOperator && c.opts.NetworkStatus != nil && ctx.Err() == nil {
			in.Permission = c.opts.NetworkStatus()
		}
		report = c.deps.checkup(ctx, in)
	} else {
		report.FinishedAt = time.Now()
	}
	if ctx.Err() != nil {
		report.Complete = false
		report.Canceled = errors.Is(ctx.Err(), context.Canceled)
		report.TimedOut = errors.Is(ctx.Err(), context.DeadlineExceeded)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.check = nil
	c.state.Checking = false
	current = !c.closed && c.ctx.Err() == nil && epoch == c.stopEpoch && cfg.Equal(c.state.Config) && c.run == g
	if current && g != nil {
		current = !g.stopping && g.ctx.Err() == nil && sameCheckRuntime(pending.sample, g.runtime.Snapshot())
	}
	if !current {
		c.state.Checkup = checkup.Report{}
		if c.state.Notice == "Checking saved connection settings…" {
			c.successLocked("Connection checks canceled: connection settings changed.")
		} else {
			c.publishLocked()
		}
		return context.Canceled
	}
	c.state.Checkup = report
	switch {
	case report.Canceled:
		c.successLocked("Connection checks canceled.")
		return context.Canceled
	case report.TimedOut:
		c.successLocked("Connection checks timed out. Partial results are shown.")
		return nil
	default:
		c.successLocked("Connection checks finished. Review warnings and skipped items.")
		return nil
	}
}

// CancelChecks leaves the peer session and diagnostic connections untouched.
// Cancellation continues even when this caller abandons its wait.
func (c *Controller) CancelChecks(ctx context.Context) error {
	c.mu.Lock()
	pending := c.check
	if pending != nil {
		pending.cancel()
	}
	c.mu.Unlock()
	if pending == nil {
		return nil
	}
	return wait(ctx, pending.done)
}

func sameCheckRuntime(a, b app.Snapshot) bool {
	return a.Generation == b.Generation && a.Phase == b.Phase && a.Peer == b.Peer &&
		a.NIC == b.NIC && a.Gateway == b.Gateway && a.Direct.Address == b.Direct.Address && a.Direct.Phase == b.Direct.Phase
}

func (c *Controller) checkEndpoints(ctx context.Context, cfg settings.Config, code string) (transport.PeerInfo, string) {
	code = strings.TrimSpace(code)
	if code != "" {
		input, err := invite.ParseInput(code)
		switch {
		case errors.Is(err, invite.ErrInvalidHost):
			return transport.PeerInfo{}, checkup.DirectInputInvalidHost
		case errors.Is(err, invite.ErrInvalidPort):
			return transport.PeerInfo{}, checkup.DirectInputInvalidPort
		case errors.Is(err, invite.ErrInvalidCode):
			return transport.PeerInfo{}, checkup.DirectInputInvalidCode
		case err != nil:
			return transport.PeerInfo{}, "invite_invalid"
		case input.Kind == invite.Direct:
			return transport.PeerInfo{}, checkup.DirectInputValid
		case input.Kind == invite.Center:
			return transport.PeerInfo{}, checkup.CenterInput
		}
		i := input.Invite
		if i.Peer.ID.IsZero() || i.Token == ([16]byte{}) || len(i.Peer.Endpoints) == 0 {
			return transport.PeerInfo{}, "invite_invalid"
		}
		return i.Peer, ""
	}
	if cfg.Client.Peer == "" {
		return transport.PeerInfo{}, "pairing_missing"
	}
	id, err := identity.ParseFingerprint(cfg.Client.Peer)
	if err != nil || id.IsZero() {
		return transport.PeerInfo{}, "pairing_invalid"
	}
	peer, err := c.deps.readPeer(ctx, c.roleDir(settings.RoleClient), id)
	if errors.Is(err, trust.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return transport.PeerInfo{}, "pairing_missing"
	}
	if err != nil {
		return transport.PeerInfo{}, "pairing_unavailable"
	}
	if peer.ID != id || len(peer.Endpoints) == 0 {
		return transport.PeerInfo{}, "pairing_invalid"
	}
	return peer, ""
}
