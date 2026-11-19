package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"slices"
	"strings"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/observe"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	"remote-enet/internal/ticket"
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
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			cfg.Logger.Warn("client stopped", "error", err)
		}
		r.finish(err)
	}()
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
	operatorID := "pending"
	if !peer.ID.IsZero() {
		operatorID = peer.ID.String()
	}
	cfg.Logger.Info("client ready", "device", id.Fingerprint().String(), "operator", operatorID)
	return r.clientLoop(ctx, cfg, connector, peer, token, store)
}

func clientPeer(ctx context.Context, cfg ClientConfig, store *trust.Store) (transport.PeerInfo, [16]byte, error) {
	var token [16]byte
	if cfg.DirectPair != nil {
		return transport.PeerInfo{}, token, nil
	}
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

type clientConnector interface {
	transport.Connector
	DialBootstrap(context.Context, string) (transport.Session, identity.Fingerprint, error)
}

// Expired credentials never reach discovery or the connector. Other saved
// endpoints remain eligible when a peer has more than one route.
func dialCandidates(peer transport.PeerInfo, now time.Time) (transport.PeerInfo, error) {
	eps := slices.DeleteFunc(slices.Clone(peer.Endpoints), func(ep transport.Endpoint) bool {
		if ep.Ticket == "" {
			return false
		}
		_, claims, err := ticket.Inspect([]byte(ep.Ticket))
		return err != nil || !claims.Expires.After(now)
	})
	if len(eps) == 0 && len(peer.Endpoints) > 0 {
		return peer, HubAuthorizationExpired
	}
	peer.Endpoints = eps
	return peer, nil
}

func (r *Runtime) clientLoop(ctx context.Context, cfg ClientConfig, connector clientConnector, peer transport.PeerInfo, token [16]byte, store *trust.Store) error {
	delay := cfg.RetryMin
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !cfg.Peer.IsZero() {
			if _, err := dialCandidates(peer, time.Now()); err != nil {
				return err
			}
		}
		nic, err := selectNIC(ctx, cfg)
		if errors.Is(err, carside.ErrNICUnavailable) {
			r.update(func(s *Snapshot) {
				s.NIC, s.Gateway, s.VIN = carside.NIC{}, netip.Addr{}, ""
			})
			if err := r.clientWait(ctx, cfg, PhaseWaitingNIC, err.Error(), cfg.NICCheckInterval); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, errEnumeration) {
				return fmt.Errorf("select vehicle interface; specify --nic and --nic-addr if ambiguous: %w", err)
			}
			if err := r.clientWait(ctx, cfg, PhaseBackoff, err.Error(), delay); err != nil {
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
			if err := r.clientWait(ctx, cfg, PhaseBackoff, fmt.Sprintf("discover vehicle: %v", err), delay); err != nil {
				return err
			}
			delay = nextDelay(delay, cfg.RetryMax)
			continue
		}
		if len(gateways) > 1 {
			return fmt.Errorf("found %d gateways; isolate one vehicle or use a unicast --discovery-target", len(gateways))
		}
		if len(gateways) == 0 {
			if err := r.clientWait(ctx, cfg, PhaseWaitingVehicle, "no gateway found; waiting for vehicle", cfg.DiscoveryRetryInterval); err != nil {
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
			if err := r.clientWait(ctx, cfg, PhaseBackoff, err.Error(), delay); err != nil {
				return err
			}
			delay = nextDelay(delay, cfg.RetryMax)
			continue
		}
		gateway := gateways[0]
		r.update(func(s *Snapshot) {
			s.Phase, s.Gateway, s.VIN = PhaseConnecting, gateway.Address, gateway.Identification.VIN
		})
		saved, connected, fatal, err := r.clientConnection(ctx, connector, peer, token, store, nic, gateway, cfg)
		if saved != nil {
			peer, cfg.DirectPair, cfg.Peer = *saved, nil, saved.ID
		} else if cfg.DirectPair != nil && !errors.Is(err, hub.ErrUnknownPeer) && !errors.Is(err, hub.ErrPeerBusy) {
			return fmt.Errorf("%w: %w", classifyPairingError(err), err)
		}
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
		message := sessionEndMessage(session.RoleClient, err)
		switch {
		case errors.Is(err, hub.ErrUnknownPeer):
			message = string(OperatorOffline)
		case errors.Is(err, identity.ErrFingerprintMismatch):
			message = string(PairingErrorKind("identity_mismatch"))
		case errors.Is(err, hubtransport.ErrPeerBusy):
			message = string(PairingErrorKind("busy"))
		}
		if err := r.clientWait(ctx, cfg, PhaseBackoff, message, delay); err != nil {
			return err
		}
		delay = nextDelay(delay, cfg.RetryMax)
	}
}

func ticketDialError(err error) (fatal bool, classified error) {
	var kind PairingErrorKind
	switch {
	case errors.Is(err, hub.ErrPeerBusy):
		kind = "busy"
	case errors.Is(err, hub.ErrUnknownPeer):
		kind = OperatorOffline
	case errors.As(err, &kind):
		return true, err
	case errors.Is(err, identity.ErrFingerprintMismatch):
		kind, fatal = HubIdentityMismatch, true
	case allEndpointsUnauthorized(err):
		kind, fatal = HubAuthorizationExpired, true
	default:
		kind = HubUnreachable
	}
	return fatal, fmt.Errorf("%w: %w", kind, err)
}

// Dial wraps the joined candidate errors. One retryable path is enough
// to keep a saved peer reachable even when another path's ticket is revoked.
func allEndpointsUnauthorized(err error) bool {
	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return errors.Is(err, hub.ErrUnauthorized)
	}
	for _, cause := range joined.Unwrap() {
		if !allEndpointsUnauthorized(cause) {
			return false
		}
	}
	return true
}

func selectNIC(ctx context.Context, cfg ClientConfig) (carside.NIC, error) {
	nics, err := cfg.Hooks.Interfaces(ctx)
	if err != nil {
		return carside.NIC{}, fmt.Errorf("%w: %w", errEnumeration, err)
	}
	return carside.SelectNICFrom(nics, cfg.NICName, cfg.NICAddr)
}

func (r *Runtime) clientWait(ctx context.Context, cfg ClientConfig, phase Phase, message string, delay time.Duration) error {
	r.update(func(s *Snapshot) {
		s.Phase, s.NextRetry = phase, time.Now().Add(delay)
		s.LastError = message
	})
	return cfg.Hooks.Wait(ctx, delay)
}

func nextDelay(delay, maximum time.Duration) time.Duration {
	if delay >= maximum/2 {
		return maximum
	}
	return delay * 2
}

func (r *Runtime) clientConnection(ctx context.Context, connector clientConnector, peer transport.PeerInfo, token [16]byte, store *trust.Store, nic carside.NIC, gateway carside.Gateway, cfg ClientConfig) (saved *transport.PeerInfo, connected, fatal bool, err error) {
	direct := cfg.DirectPair
	cfg.DirectPair = nil
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
	var ts transport.Session
	var outer, pairingHub identity.Fingerprint
	if direct != nil {
		ts, outer, err = connector.DialBootstrap(connectionCtx, direct.Address)
		if err == nil && ts.Peer() != outer {
			pairingHub = outer
		}
	} else {
		candidates := peer
		if !cfg.Peer.IsZero() {
			candidates, err = dialCandidates(peer, time.Now())
			if err != nil {
				return nil, false, true, err
			}
		}
		ts, err = connector.Dial(connectionCtx, candidates)
		if err != nil && connectionCtx.Err() == nil && slices.ContainsFunc(candidates.Endpoints, func(ep transport.Endpoint) bool { return ep.Ticket != "" }) {
			fatal, err := ticketDialError(err)
			return nil, false, fatal, err
		}
	}
	if err != nil {
		return nil, false, false, err
	}
	path := ts.Path()
	requiresTicket := !pairingHub.IsZero() || slices.ContainsFunc(peer.Endpoints, func(ep transport.Endpoint) bool {
		return ep.Hub == path.Hub && ep.Addr == path.Via && ep.Ticket != ""
	})
	forwarder, err := carside.NewForwarder(nic, cfg.Logger, carside.WithObserver(metrics))
	if err != nil {
		ts.Close(err)
		return nil, false, true, err
	}
	defer func() {
		cancel(context.Canceled)
		ts.Close(err)
		forwarder.Close()
		metrics.end(err)
		r.deactivate()
	}()
	var code string
	if direct != nil {
		code = direct.Code
		direct.Code = "" // defaults made a private copy for this run.
	}
	s, err := session.Run(connectionCtx, ts, session.Options{
		Role: session.RoleClient, Initiator: true, AppVersion: "dev", Token: token,
		HubFP: pairingHub, EndpointHub: path.Hub, RequireHubTicket: requiresTicket,
		DigitalCode: code, LocalFP: r.Snapshot().Device, OnTunnel: forwarder.HandleTunnel,
		OnRTT: metrics.onRTT, AllowLinkTest: cfg.AllowLinkTest, Logger: cfg.Logger,
	})
	if err != nil {
		if errors.Is(err, session.ReasonHubTicket) {
			return nil, false, true, fmt.Errorf("%w: %w", PairingErrorKind(session.ReasonHubTicket), err)
		}
		return nil, false, false, err
	}
	if direct != nil {
		if s.PairingOutcome() != session.DigitalPaired {
			return nil, false, true, session.ReasonInterrupted
		}
		peer = transport.PeerInfo{ID: ts.Peer(), Endpoints: []transport.Endpoint{{Kind: "hub", Addr: direct.Address, Hub: outer}}}
	} else {
		metrics.start()
	}
	peer, err = persistPeer(connectionCtx, store, peer, path, string(s.HubTicket()), !cfg.Peer.IsZero())
	if err != nil {
		if connectionCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
			return nil, false, false, context.Cause(connectionCtx)
		}
		if direct != nil {
			return nil, false, true, cpace.ReasonStoreFailed
		}
		return saved, true, true, fmt.Errorf("save paired operator: %w", err)
	}
	saved = &peer
	if direct != nil {
		metrics.start()
		if cfg.Hooks.OnPaired != nil {
			copyPeer := peer
			copyPeer.Endpoints = append([]transport.Endpoint(nil), peer.Endpoints...)
			cfg.Hooks.OnPaired(copyPeer)
		}
	}
	if err := s.Send(gateway.VehicleState(nic, 1)); err != nil {
		return saved, true, false, err
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
			return saved, true, false, cause
		}
		return saved, true, false, s.Err()
	}
}

// Each device ticket is bound to one Hub. An unused bearer credential from
// a multi-endpoint invitation must never enter the saved peer record.
func persistPeer(ctx context.Context, store *trust.Store, peer transport.PeerInfo, path transport.PathInfo, deviceTicket string, reconnect bool) (transport.PeerInfo, error) {
	if reconnect && deviceTicket == "" {
		return peer, nil
	}
	endpoints := make([]transport.Endpoint, 0, len(peer.Endpoints))
	matched := false
	for _, ep := range peer.Endpoints {
		if deviceTicket != "" && ep.Hub == path.Hub && ep.Addr == path.Via {
			ep.Ticket = deviceTicket
			matched = true
		} else if !reconnect && ep.Ticket != "" {
			continue
		}
		endpoints = append(endpoints, ep)
	}
	if deviceTicket != "" && !matched {
		return peer, PairingErrorKind(session.ReasonHubTicket)
	}
	peer.Endpoints = endpoints
	if reconnect {
		return peer, store.UpdateEndpointTicket(ctx, peer.ID, path.Hub, path.Via, deviceTicket)
	}
	return peer, store.SavePeer(ctx, peer)
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
