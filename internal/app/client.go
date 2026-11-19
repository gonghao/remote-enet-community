package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"strings"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/invite"
	"remote-enet/internal/observe"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

// ErrNICChanged ends the old generation before rediscovering with a fresh source.
var ErrNICChanged = errors.New("app: vehicle interface address or prefix changed")

var errEnumeration = errors.New("app: enumerate vehicle interfaces")

// RunClient waits for the selected interface and a unique gateway. Pairing,
// storage and configuration failures stop it; network failures retry with a
// bounded delay. Returning guarantees that the old forwarder has been joined.
func (r *Runtime) RunClient(ctx context.Context, cfg ClientConfig) (err error) {
	if err := r.begin(session.RoleClient); err != nil {
		return err
	}
	defer func() { r.finish(err) }()
	if err := cfg.defaults(); err != nil {
		return err
	}
	store, err := trust.Open(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	peer, token, err := clientPeer(ctx, cfg, store)
	if err != nil {
		return err
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		return err
	}
	connector, _, err := hubtransport.New(hubtransport.Options{Local: id, Wrap: wrap(id)})
	if err != nil {
		return err
	}
	r.update(func(s *Snapshot) { s.Device, s.Peer = id.Fingerprint(), peer.ID })
	cfg.Logger.Info("client ready", "device", id.Fingerprint().String(), "operator", peer.ID.String())
	return r.clientLoop(ctx, cfg, connector, peer, token, store)
}

func clientPeer(ctx context.Context, cfg ClientConfig, store *trust.Store) (transport.PeerInfo, [16]byte, error) {
	var token [16]byte
	if !cfg.Peer.IsZero() {
		peer, err := store.Peer(ctx, cfg.Peer)
		if err != nil {
			return peer, token, fmt.Errorf("load paired operator; use a new invitation for first pairing: %w", err)
		}
		if err := validatePeer(peer); err != nil {
			return peer, token, fmt.Errorf("stored operator: %w", err)
		}
		return peer, token, nil
	}
	code := cfg.Invite
	if cfg.InviteFile != "" {
		raw, err := os.ReadFile(cfg.InviteFile)
		if err != nil {
			return transport.PeerInfo{}, token, fmt.Errorf("read invitation file: %w", err)
		}
		code = strings.TrimSpace(strings.TrimPrefix(string(raw), "\uFEFF"))
	}
	i, err := invite.Decode(code)
	if err != nil {
		return transport.PeerInfo{}, token, fmt.Errorf("decode invitation: %w", err)
	}
	if i.Peer.ID.IsZero() || i.Token == token || len(i.Peer.Endpoints) == 0 {
		return transport.PeerInfo{}, token, fmt.Errorf("invitation requires an operator identity, endpoints, and a nonzero token")
	}
	if err := validatePeer(i.Peer); err != nil {
		return transport.PeerInfo{}, token, fmt.Errorf("invitation endpoint: %w", err)
	}
	return i.Peer, i.Token, nil
}

func validatePeer(peer transport.PeerInfo) error {
	if peer.ID.IsZero() || len(peer.Endpoints) == 0 {
		return fmt.Errorf("operator identity and endpoints required")
	}
	for _, ep := range peer.Endpoints {
		if ep.Kind != "hub" || ep.Hub.IsZero() {
			return fmt.Errorf("pinned hub endpoints required")
		}
		if err := endpoint(ep.Addr); err != nil {
			return err
		}
	}
	return nil
}

func (r *Runtime) clientLoop(ctx context.Context, cfg ClientConfig, connector transport.Connector, peer transport.PeerInfo, token [16]byte, store *trust.Store) error {
	delay := cfg.RetryMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		nic, err := selectNIC(ctx, cfg)
		if errors.Is(err, carside.ErrNICUnavailable) {
			r.update(func(s *Snapshot) {
				s.NIC, s.Gateway, s.VIN = carside.NIC{}, netip.Addr{}, ""
			})
			if err := r.clientWait(ctx, cfg, PhaseWaitingNIC, err, cfg.NICCheckInterval); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, errEnumeration) {
				return fmt.Errorf("select vehicle interface; specify --nic and --nic-addr if ambiguous: %w", err)
			}
			if err := r.clientWait(ctx, cfg, PhaseBackoff, err, delay); err != nil {
				return err
			}
			delay = nextDelay(delay, cfg.RetryMax)
			continue
		}
		r.update(func(s *Snapshot) {
			s.Phase, s.NIC, s.Gateway, s.VIN = PhaseDiscovering, nic, netip.Addr{}, ""
			s.NextRetry = time.Time{}
		})
		gateways, err := cfg.Hooks.Discover(ctx, carside.DiscoverOptions{NIC: nic, Destinations: cfg.DiscoveryDestinations, Timeout: cfg.DiscoveryTimeout, Logger: cfg.Logger})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := r.clientWait(ctx, cfg, PhaseBackoff, fmt.Errorf("discover vehicle: %w", err), delay); err != nil {
				return err
			}
			delay = nextDelay(delay, cfg.RetryMax)
			continue
		}
		if len(gateways) > 1 {
			return fmt.Errorf("found %d gateways; isolate one vehicle or use a unicast --discovery-target", len(gateways))
		}
		if len(gateways) == 0 {
			if err := r.clientWait(ctx, cfg, PhaseWaitingVehicle, errors.New("no gateway found; waiting for vehicle"), cfg.DiscoveryRetryInterval); err != nil {
				return err
			}
			continue
		}
		// Discovery may outlive an address change. Never dial using its stale source.
		fresh, err := selectNIC(ctx, cfg)
		if errors.Is(err, carside.ErrNICUnavailable) || err == nil && fresh != nic {
			continue
		}
		if err != nil {
			if !errors.Is(err, errEnumeration) {
				return err
			}
			if err := r.clientWait(ctx, cfg, PhaseBackoff, err, delay); err != nil {
				return err
			}
			delay = nextDelay(delay, cfg.RetryMax)
			continue
		}
		gateway := gateways[0]
		r.update(func(s *Snapshot) {
			s.Phase, s.Gateway, s.VIN = PhaseConnecting, gateway.Address, gateway.Identification.VIN
		})
		connected, fatal, err := r.clientConnection(ctx, connector, peer, token, store, nic, gateway, cfg)
		if connected {
			token = [16]byte{}
			delay = cfg.RetryMin
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatal || errors.Is(err, session.ErrPairing) || errors.Is(err, session.ErrVersion) {
			return err
		}
		if errors.Is(err, ErrNICChanged) || errors.Is(err, carside.ErrNICUnavailable) {
			r.update(func(s *Snapshot) { s.LastError = err.Error() })
			continue
		}
		cfg.Logger.Warn("client connection ended; retrying", "error", err, "retry_in", delay)
		if err := r.clientWait(ctx, cfg, PhaseBackoff, err, delay); err != nil {
			return err
		}
		delay = nextDelay(delay, cfg.RetryMax)
	}
}

func selectNIC(ctx context.Context, cfg ClientConfig) (carside.NIC, error) {
	nics, err := cfg.Hooks.Interfaces(ctx)
	if err != nil {
		return carside.NIC{}, fmt.Errorf("%w: %w", errEnumeration, err)
	}
	return carside.SelectNICFrom(nics, cfg.NICName, cfg.NICAddr)
}

func (r *Runtime) clientWait(ctx context.Context, cfg ClientConfig, phase Phase, cause error, delay time.Duration) error {
	r.update(func(s *Snapshot) {
		s.Phase, s.NextRetry = phase, time.Now().Add(delay)
		if cause != nil {
			s.LastError = cause.Error()
		}
	})
	return cfg.Hooks.Wait(ctx, delay)
}

func nextDelay(delay, maximum time.Duration) time.Duration {
	if delay >= maximum/2 {
		return maximum
	}
	return delay * 2
}

func (r *Runtime) clientConnection(ctx context.Context, connector transport.Connector, peer transport.PeerInfo, token [16]byte, store *trust.Store, nic carside.NIC, gateway carside.Gateway, cfg ClientConfig) (connected, fatal bool, err error) {
	connectionCtx, cancel := context.WithCancelCause(ctx)
	metrics := &measurements{runtime: r, direction: observe.Inbound}
	monitorDone := make(chan error, 1)
	go func() {
		err := r.monitorNIC(connectionCtx, nic, cfg)
		if err != nil {
			cancel(err)
		}
		monitorDone <- err
	}()
	defer func() {
		cancel(context.Canceled)
		if monitorErr := <-monitorDone; monitorErr != nil && ctx.Err() == nil && !fatal && !errors.Is(err, session.ErrPairing) && !errors.Is(err, session.ErrVersion) {
			err = monitorErr
			fatal = !errors.Is(monitorErr, ErrNICChanged) && !errors.Is(monitorErr, carside.ErrNICUnavailable)
		}
	}()
	ts, err := connector.Dial(connectionCtx, peer)
	if err != nil {
		return false, false, err
	}
	forwarder, err := carside.NewForwarder(nic, cfg.Logger, carside.WithObserver(metrics))
	if err != nil {
		ts.Close(err)
		return false, true, err
	}
	defer func() {
		cancel(context.Canceled)
		ts.Close(err)
		forwarder.Close()
		metrics.end(err)
		r.deactivate()
	}()
	s, err := session.Run(connectionCtx, ts, session.Options{Role: session.RoleClient, Initiator: true, AppVersion: "dev", Token: token, OnTunnel: forwarder.HandleTunnel, OnRTT: metrics.onRTT, AllowLinkTest: cfg.AllowLinkTest, Logger: cfg.Logger})
	if err != nil {
		return false, false, err
	}
	metrics.start()
	if err := store.SavePeer(connectionCtx, peer); err != nil {
		if connectionCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return true, false, context.Cause(connectionCtx)
		}
		return true, true, fmt.Errorf("save paired operator: %w", err)
	}
	if err := s.Send(gateway.VehicleState(nic, 1)); err != nil {
		return true, false, err
	}
	generation := r.activate(connectionCtx, s, ts.Path())
	cfg.Logger.Info("client connected", "operator", peer.ID.String(), "path", ts.Path().Kind, "gateway", gateway.Address.String(), "vin", gateway.Identification.VIN, "generation", generation)
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-connectionCtx.Done():
		case <-s.Done():
		case <-tick.C:
			r.update(func(*Snapshot) {})
			continue
		}
		// The session may close with ctx.Err() before this loop observes
		// cancellation. Prefer the cause regardless of which channel won.
		if cause := context.Cause(connectionCtx); cause != nil {
			return true, false, cause
		}
		return true, false, s.Err()
	}
}

func (r *Runtime) monitorNIC(ctx context.Context, selected carside.NIC, cfg ClientConfig) error {
	for {
		if err := cfg.Hooks.Wait(ctx, cfg.NICCheckInterval); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("wait for vehicle interface check: %w", err)
		}
		fresh, err := selectNIC(ctx, cfg)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.Is(err, errEnumeration) {
				// An enumeration failure alone is not evidence of a source change.
				r.update(func(s *Snapshot) { s.LastError = err.Error() })
				continue
			}
			return err
		}
		if fresh != selected {
			return ErrNICChanged
		}
	}
}
