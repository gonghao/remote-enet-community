package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/observe"
	"remote-enet/internal/operator"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

// OperatorConfig keeps listener configuration separate from per-session state.
// Zero local ports are ephemeral; zero remote ports use HSFZ defaults.
type OperatorConfig struct {
	StateDir             string
	Listen               string
	Hub                  *HubConfig
	ShareAddress         string
	Bind                 netip.Addr
	Ports                operator.Ports
	RemoteDiagnosticPort uint16
	RemoteControlPort    uint16
	Logger               *slog.Logger
	AllowLinkTest        bool
	Exclusive            bool
	Resolver             exclusive.Resolver
	DirectFactory        DirectFactory
	AutoMGU              bool
	AutoEVO              bool
	// Capture is caller-owned and observes only HSFZ presentation and MGU queries.
	Capture *hsfzcapture.Recorder
}

func (cfg OperatorConfig) validate() error {
	if cfg.AutoMGU && cfg.AutoEVO {
		return fmt.Errorf("app: automatic MGU and EVO direct access are mutually exclusive")
	}
	if cfg.Exclusive && cfg.Resolver == nil && runtime.GOOS != "windows" {
		return fmt.Errorf("app: exclusive mode requires Windows or an injected resolver: %w", exclusive.ErrUnsupported)
	}
	if cfg.StateDir == "" {
		return fmt.Errorf("app: operator state directory required")
	}
	if !unicastIPv4(cfg.Bind) {
		return fmt.Errorf("app: operator bind must be unicast IPv4")
	}
	if cfg.Hub != nil {
		if cfg.Listen != "" {
			return fmt.Errorf("app: listen and hub are mutually exclusive")
		}
		if err := cfg.Hub.validate(); err != nil {
			return err
		}
	} else {
		_, p, err := net.SplitHostPort(cfg.Listen)
		if err != nil {
			return fmt.Errorf("app: operator listen must be host:port")
		}
		n, err := strconv.ParseUint(p, 10, 16)
		if err != nil || n == 0 {
			return fmt.Errorf("app: operator listen port must be in 1..65535")
		}
	}
	for _, p := range []int{cfg.Ports.Diagnostic, cfg.Ports.Control, cfg.Ports.Discovery} {
		if p < 0 || p > 65535 {
			return fmt.Errorf("app: operator local ports must be in 0..65535")
		}
	}
	return nil
}

// RunOperator serves one session at a time. Each presenter is closed and joined
// before the next Accept, so a reconnect cannot inherit old sockets or workers.
func (r *Runtime) RunOperator(ctx context.Context, cfg OperatorConfig) (err error) {
	if err := r.begin(session.RoleOperator); err != nil {
		return err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	defer func() {
		if err != nil && !errors.Is(err, context.Canceled) {
			cfg.Logger.Warn("operator stopped", "error", err)
		}
		r.finish(err)
	}()
	if err := cfg.validate(); err != nil {
		return err
	}
	if cfg.ShareAddress != "" {
		cfg.ShareAddress, err = invite.ParseAddress(cfg.ShareAddress)
		if err != nil {
			return err
		}
	}
	r.update(func(s *Snapshot) { s.Exclusive.Enabled = cfg.Exclusive })
	store, err := trust.Open(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		return err
	}
	opts := hubtransport.Options{Local: id, Wrap: wrap(id), Listen: cfg.Listen}
	if cfg.Hub != nil {
		if cfg.Hub.FP == id.Fingerprint() {
			return fmt.Errorf("app: standalone hub requires a separate identity")
		}
		if cfg.ShareAddress == "" {
			cfg.ShareAddress, _ = invite.ParseAddress(cfg.Hub.Addr)
		}
		opts.Endpoints = []transport.Endpoint{{Kind: "hub", Addr: cfg.Hub.Addr, Hub: cfg.Hub.FP}}
	}
	manager := pairing.NewManager(pairing.Config{Address: cfg.ShareAddress})
	responder := &digitalResponder{Manager: manager, listener: ctx}
	if cfg.Hub != nil {
		issuer, err := loadIssuer(cfg.StateDir)
		if err != nil {
			return err
		}
		hubFP := cfg.Hub.FP
		responder.grant = func(client identity.Fingerprint) ([]byte, error) {
			return issuer.Dial(time.Now(), client, id.Fingerprint(), hubFP)
		}
		opts.WaitToken = func(ctx context.Context, hub identity.Fingerprint) ([]byte, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			status := manager.Status()
			var until time.Time
			if status.Reason == "" {
				until = status.ExpiresAt
			}
			raw, err := issuer.Wait(time.Now(), id.Fingerprint(), hub, until)
			responder.waitToken = string(raw)
			responder.waitGeneration = status.Generation
			return raw, err
		}
		opts.OnRegister = func(_ identity.Fingerprint, token []byte, err error) { r.onRegister(responder, token, err) }
	}
	connector, acceptor, err := hubtransport.New(opts)
	if err != nil {
		return err
	}
	if cfg.Hub != nil {
		responder.refresh = connector.Refresh
	}
	stopClose := context.AfterFunc(ctx, manager.Close)
	defer func() {
		stopClose()
		manager.Close()
		_ = acceptor.Close()
	}()
	r.mu.Lock()
	r.numeric = responder
	r.mu.Unlock()
	r.update(func(s *Snapshot) { s.Device = id.Fingerprint() })
	cfg.Logger.Info("operator ready", "device", id.Fingerprint().String(), "listen", cfg.Listen, "bind", cfg.Bind.String())
	return r.operatorLoop(ctx, acceptor, cfg, store)
}

func (r *Runtime) operatorLoop(ctx context.Context, acceptor transport.Acceptor, cfg OperatorConfig, store *trust.Store) error {
	delay := 100 * time.Millisecond
	for {
		r.update(func(s *Snapshot) { s.Phase, s.NextRetry = PhaseListening, time.Time{} })
		ts, err := acceptor.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if cfg.Hub != nil && errors.Is(err, identity.ErrFingerprintMismatch) {
				return fmt.Errorf("%w: %w", HubIdentityMismatch, err)
			}
			if errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed) {
				return fmt.Errorf("operator hub stopped: %w", err)
			}
			cfg.Logger.Warn("operator connection rejected", "error", err)
			r.update(func(s *Snapshot) {
				s.HubWaiting = false
				s.Phase, s.LastError, s.NextRetry = PhaseBackoff, "Client connection could not be accepted.", time.Now().Add(delay)
			})
			if err := pause(ctx, delay); err != nil {
				return err
			}
			delay = nextDelay(delay, 5*time.Second)
			continue
		}
		delay = 100 * time.Millisecond
		r.update(func(s *Snapshot) {
			r.numeric.waitToken = ""
			s.HubWaiting = false
			s.Phase, s.LastError = PhaseConnecting, ""
		})
		fatal, err := r.operatorConnection(ctx, ts, cfg, store, cfg.Logger)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if fatal {
			return err
		}
		r.update(func(s *Snapshot) { s.LastError = sessionEndMessage(session.RoleOperator, err) })
	}
}

func setVehicle(s *Snapshot, state proto.VehicleState) {
	s.NIC = carside.NIC{Name: state.NIC}
	s.NIC.Address, _ = netip.ParseAddr(state.NICAddr)
	s.Gateway, _ = netip.ParseAddr(state.GatewayAddr)
	s.VIN = state.VIN
}

func (r *Runtime) operatorConnection(ctx context.Context, ts transport.Session, cfg OperatorConfig, store *trust.Store, logger *slog.Logger) (fatal bool, err error) {
	connectionCtx, cancel := context.WithCancel(ctx)
	metrics := &measurements{runtime: r, direction: observe.Outbound}
	var peer identity.Fingerprint
	defer func() {
		cancel()
		ts.Close(err)
		metrics.end(err)
		r.deactivate()
		if ctx.Err() == nil && !fatal {
			if peer.IsZero() {
				logger.Info("operator session ended", "error", err)
			} else {
				logger.Info("operator session ended", "peer", peer.String(), "error", err)
			}
		}
	}()
	states := make(chan proto.VehicleState, 1)
	var authErr error
	var responder session.DigitalResponder
	var grant func(identity.Fingerprint) ([]byte, error)
	var hubFP identity.Fingerprint
	r.mu.Lock()
	if r.numeric != nil {
		responder, grant = r.numeric, r.numeric.grant
	}
	localFP := r.state.Device
	r.mu.Unlock()
	if cfg.Hub != nil {
		hubFP = cfg.Hub.FP
	}
	s, err := session.Run(connectionCtx, ts, session.Options{
		HubFP: hubFP, GrantTicket: grant,
		Role: session.RoleOperator, AppVersion: "dev", Logger: logger, AllowLinkTest: cfg.AllowLinkTest, OnRTT: metrics.onRTT,
		DigitalManager: responder, LocalFP: localFP, Remember: store.Remember,
		Trusted: func(peer identity.Fingerprint) bool {
			ok, err := store.Trusted(connectionCtx, peer)
			if err != nil {
				authErr = err
				cancel()
			}
			return err == nil && ok
		},
		AcceptToken: func(peer identity.Fingerprint, token [16]byte) bool {
			if authErr != nil {
				return false
			}
			ok, err := store.Consume(connectionCtx, peer, token)
			if err != nil {
				authErr = err
				cancel()
			}
			return err == nil && ok
		},
		OnMessage: func(msg any) {
			if state, ok := msg.(proto.VehicleState); ok {
				state.IdentRaw = bytes.Clone(state.IdentRaw)
				select {
				case states <- state:
				default:
					select {
					case <-states:
					default:
					}
					select {
					case states <- state:
					default:
					}
				}
			}
		},
	})
	if authErr != nil {
		return true, fmt.Errorf("operator authentication state: %w", authErr)
	}
	if err != nil {
		var reason cpace.Reason
		if errors.As(err, &reason) {
			return false, fmt.Errorf("%w: %w", classifyPairingError(err), err)
		}
		return false, err
	}
	captureGeneration := metrics.start()
	var first proto.VehicleState
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case first = <-states:
	case <-s.Done():
		return false, s.Err()
	case <-ctx.Done():
		return false, ctx.Err()
	case <-timer.C:
		return false, fmt.Errorf("vehicle state not received within 10s")
	}
	if first.GatewayAddr == "" || len(first.IdentRaw) == 0 {
		return false, fmt.Errorf("initial vehicle state requires a gateway and identification datagram")
	}
	var gate *exclusive.Gate
	if cfg.Exclusive {
		gate = exclusive.NewGate(cfg.Resolver)
	}
	p, err := operator.Start(connectionCtx, operator.Options{
		Bind: cfg.Bind, Ports: cfg.Ports,
		RemoteDiagnosticPort: cfg.RemoteDiagnosticPort, RemoteControlPort: cfg.RemoteControlPort,
		Opener: s, Logger: logger, Exclusive: cfg.Exclusive, Gate: gate, Resolver: cfg.Resolver,
		Observer: metrics, Capture: cfg.Capture,
		CaptureGeneration: func() uint64 { return captureGeneration },
	})
	if err != nil {
		return true, fmt.Errorf("start diagnostic listeners: %w", err)
	}
	defer func() { cancel(); ts.Close(err); p.Close() }()
	r.mu.Lock()
	r.exclusiveState = p.ExclusiveState
	r.mu.Unlock()
	if err := p.Update(first); err != nil {
		return false, fmt.Errorf("invalid initial vehicle state: %w", err)
	}
	diag, ctrl, disc := p.Addr()
	r.update(func(s *Snapshot) {
		setVehicle(s, first)
		s.Diagnostic, s.Control, s.Discovery = diag.AddrPort(), ctrl.AddrPort(), disc.AddrPort()
	})
	factory := cfg.DirectFactory
	if factory == nil {
		factory = nativeDirectFactory
	}
	port := cfg.RemoteDiagnosticPort
	if port == 0 {
		port = 6801
	}
	newManager := func() *directManager {
		return &directManager{
			runtime: r, ctx: connectionCtx, presenter: p, opener: s, factory: factory,
			observer: metrics, gate: gate, port: port,
			queryOpener: captureQueryOpener{
				opener: s, recorder: cfg.Capture, generation: func() uint64 { return captureGeneration },
			},
		}
	}
	manager := newManager()
	install := func() {
		r.mu.Lock()
		r.direct = manager
		r.state.Direct = DirectState{Phase: DirectDisabled}
		r.publishLocked()
		r.mu.Unlock()
	}
	install()
	generation := r.activate(connectionCtx, s, ts.Path())
	peer = s.Peer()
	autoDone := make(chan struct{})
	if cfg.AutoMGU || cfg.AutoEVO {
		initial := manager
		go func() { defer close(autoDone); _ = initial.connect(connectionCtx, cfg.AutoEVO) }()
	} else {
		close(autoDone)
	}
	defer func() {
		cancel()
		_ = manager.disconnect(true)
		<-autoDone
		r.mu.Lock()
		if r.direct == manager {
			r.direct = nil
		}
		r.mu.Unlock()
	}()

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	logger.Info("operator presenting vehicle", "peer", ts.Peer().String(), "vin", first.VIN, "diagnostic", diag.String(), "control", ctrl.String(), "discovery", disc.String(), "generation", generation)
	for {
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-s.Done():
			return false, s.Err()
		case <-tick.C:
			_ = manager.checkHealth()
			r.update(func(*Snapshot) {})
		case state := <-states:
			previous := r.Snapshot()
			next := Snapshot{}
			setVehicle(&next, state)
			changed := next.Gateway != previous.Gateway || next.NIC.Name != previous.NIC.Name || next.NIC.Address != previous.NIC.Address
			var cleanupErr error
			if changed {
				cleanupErr = manager.disconnect(true)
			}
			if err := p.Update(state); err != nil {
				return false, fmt.Errorf("update vehicle state: %w", err)
			}
			if changed && cleanupErr == nil {
				manager = newManager()
				install()
			}

			r.update(func(s *Snapshot) { setVehicle(s, state) })
		}
	}
}
