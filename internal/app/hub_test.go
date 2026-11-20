package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/session"
	"remote-enet/internal/ticket"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func startStandaloneHub(t *testing.T, operatorDir string, numeric bool) HubConfig {
	t.Helper()
	host, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := host.Save(dir); err != nil {
		t.Fatal(err)
	}
	entry, err := IssuerEntry(t.Context(), operatorDir, "operator")
	if err != nil {
		t.Fatal(err)
	}
	var issuer hubauth.Issuer
	if err := json.Unmarshal([]byte(entry), &issuer); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg := hubauth.Config{Version: 1, Mode: "selfhost", IdentityDir: dir, Listen: ln.Addr().String(), Issuers: []hubauth.Issuer{issuer}, ClockSkewS: 5, TicketMaxTTLS: 2678400, BootstrapMaxTTLS: 86400, PairingWindowMaxS: 900, SetupLimits: hubauth.SetupLimits{Concurrent: 64, PerSecond: 32, Burst: 64, TimeoutS: 10}}
	if numeric {
		cfg.PairingOperator = issuer.Operator
	}
	p, err := hubauth.NewPolicy(cfg, host.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- hub.NewServer(hub.Options{Local: host, Policy: p}).Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	})
	return HubConfig{Addr: cfg.Listen, FP: host.Fingerprint()}
}

func TestStandaloneHubPairingAndRenewal(t *testing.T) {
	for _, numeric := range []bool{false, true} {
		t.Run(map[bool]string{true: "numeric", false: "disabled-window-and-invite"}[numeric], func(t *testing.T) {
			operatorDir := t.TempDir()
			h := startStandaloneHub(t, operatorDir, numeric)
			nic, gw := gatewayState(t)
			op := New()
			ctx, cancel := context.WithCancel(t.Context())
			states := op.Subscribe(ctx)
			done := make(chan error, 1)
			go func() {
				done <- op.RunOperator(ctx, OperatorConfig{StateDir: operatorDir, Hub: &h, Bind: nic.Address, Logger: quietLogger()})
			}()
			defer func() {
				cancel()
				if err := <-done; !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}()
			waiting := func(s Snapshot) bool { return s.HubWaiting }
			initial := snapshotWhere(t, states, waiting)
			d, err := op.IssueNumericCode(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if d.Address != h.Addr {
				t.Fatal("incorrect share address")
			}
			snapshotWhere(t, states, waiting)
			cfg := connectionConfig(nic)
			cfg.StateDir = t.TempDir()
			cfg.Hooks.Discover = func(context.Context, carside.DiscoverOptions) ([]carside.Gateway, error) {
				return []carside.Gateway{gw}, nil
			}
			if numeric {
				cfg.DirectPair = &DirectPair{Address: d.Address, Code: d.Code}
			} else {
				if string(op.NumericCodeStatus().Reason) != string(HubPairingDisabled) {
					t.Fatal("missing window refusal")
				}
				if _, err := op.NumericCodeDisplay(ctx, d.Generation); err == nil {
					t.Fatal("denied code remains visible")
				}
				cfg.Invite, err = IssueInvite(ctx, InviteConfig{StateDir: operatorDir, Hub: &h, TTL: time.Minute, Logger: quietLogger()})
				if err != nil {
					t.Fatal(err)
				}
			}
			store, err := trust.Open(ctx, cfg.StateDir)
			if err != nil {
				t.Fatal(err)
			}
			var previous string
			for range 2 {
				client := New()
				cctx, stop := context.WithCancel(ctx)
				ch := client.Subscribe(cctx)
				cdone := make(chan error, 1)
				go func(cfg ClientConfig) { cdone <- client.RunClient(cctx, cfg) }(cfg)
				func() {
					defer func() {
						stop()
						if err := <-cdone; !errors.Is(err, context.Canceled) {
							t.Error(err)
						}
					}()
					s := connectedGeneration(t, ch, 1)
					if s.Path.Kind != transport.PathRelay || s.Path.Via != h.Addr || s.Path.Hub != h.FP {
						t.Fatal("incorrect client path")
					}
					peer, err := store.Peer(ctx, initial.Device)
					if err != nil || len(peer.Endpoints) != 1 {
						t.Fatal("missing persisted endpoint", err)
					}
					ep := peer.Endpoints[0]
					_, claims, err := ticket.Inspect([]byte(ep.Ticket))
					if err != nil || ep.Addr != h.Addr || ep.Hub != h.FP || claims.Bearer || claims.Device != s.Device || claims.Peer != initial.Device || claims.Hub != h.FP || ep.Ticket == previous {
						t.Fatal("incorrect persisted device ticket", err)
					}
					previous = ep.Ticket
				}()
				snapshotWhere(t, states, waiting)
				cfg.Invite, cfg.DirectPair, cfg.Peer = "", nil, initial.Device
			}
			issuer, err := loadIssuer(operatorDir)
			if err != nil {
				t.Fatal(err)
			}
			clientID, err := store.LoadIdentity(ctx)
			if err != nil {
				t.Fatal(err)
			}
			expired, err := issuer.Dial(time.Now().Add(-ticket.DialTTL-time.Minute), clientID.Fingerprint(), initial.Device, h.FP)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.UpdateEndpointTicket(ctx, initial.Device, h.FP, h.Addr, string(expired)); err != nil {
				t.Fatal(err)
			}
			cfg.Hooks.Interfaces = func(context.Context) ([]carside.NIC, error) {
				t.Fatal("expired ticket reached network discovery")
				return nil, nil
			}
			if err := New().RunClient(ctx, cfg); !errors.Is(err, HubAuthorizationExpired) {
				t.Fatal("expired ticket", err)
			}
		})
	}
}

func TestHubPairingErrors(t *testing.T) {
	for err, want := range map[error]PairingErrorKind{hub.ErrUnauthorized: "unavailable", hub.ErrPeerBusy: "busy", hub.ErrUnknownPeer: OperatorOffline, session.ReasonHubTicket: "hub_ticket"} {
		if got := classifyPairingError(err); got != want {
			t.Errorf("%v: got %v, want %v", err, got, want)
		}
	}
}

func TestWindowRefusalIgnoresSupersededRegistration(t *testing.T) {
	m := pairing.NewManager(pairing.Config{Address: "localhost:47000"})
	defer m.Close()
	refreshes := 0
	d := &digitalResponder{Manager: m, listener: t.Context(), refresh: func() { refreshes++ }}
	r := New()
	r.numeric = d
	r.state.Phase = PhaseListening
	first, err := r.IssueNumericCode(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	d.waitGeneration, d.waitToken = first.Generation, "first"
	second, err := r.IssueNumericCode(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.onRegister(d, []byte("first"), hub.ErrUnauthorized)
	d.waitGeneration, d.waitToken = second.Generation, "second"
	r.onRegister(d, []byte("first"), hub.ErrUnauthorized)
	if _, err := r.NumericCodeDisplay(t.Context(), second.Generation); err != nil || refreshes != 2 {
		t.Fatal("stale refusal revoked the replacement code", err)
	}
}

func TestTicketDialFailureKinds(t *testing.T) {
	for _, tc := range []struct {
		err   error
		kind  PairingErrorKind
		fatal bool
	}{
		{hub.ErrUnauthorized, HubAuthorizationExpired, true},
		{fmt.Errorf("all endpoints: %w", errors.Join(net.ErrClosed, hub.ErrUnauthorized)), HubUnreachable, false},
		{errors.Join(hub.ErrUnauthorized, net.ErrClosed), HubUnreachable, false},
		{errors.Join(hub.ErrUnauthorized, fmt.Errorf("relay: %w", hub.ErrUnauthorized)), HubAuthorizationExpired, true},
		{hub.ErrPeerBusy, "busy", false},
		{hub.ErrUnknownPeer, OperatorOffline, false},
		{identity.ErrFingerprintMismatch, HubIdentityMismatch, true},
		{errors.Join(PairingErrorKind("identity_mismatch"), identity.ErrFingerprintMismatch), "identity_mismatch", true},
		{net.ErrClosed, HubUnreachable, false},
	} {
		fatal, err := ticketDialError(tc.err)
		if fatal != tc.fatal || !errors.Is(err, tc.kind) || !errors.Is(err, tc.err) {
			t.Errorf("wrong retry decision or error kind for %v", tc.err)
		}
	}
}

func TestLocalIssuerFailureKind(t *testing.T) {
	r := New()
	r.state.Phase = PhaseListening
	d := &digitalResponder{listener: t.Context()}
	r.onRegister(d, nil, fmt.Errorf("sign wait ticket: %w", HubIssuer))
	if got := r.Snapshot(); got.LastError != "hub_issuer" || got.HubWaiting {
		t.Fatalf("failed registration: %v", got.LastError)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "issuer.key"), []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	r = New()
	cfg := OperatorConfig{StateDir: dir, Hub: &HubConfig{Addr: "127.0.0.1:47000", FP: identity.Fingerprint{1}}, Bind: netip.MustParseAddr("127.0.0.1"), Logger: quietLogger()}
	if err := r.RunOperator(t.Context(), cfg); !errors.Is(err, HubIssuer) || r.Snapshot().LastError != "hub_issuer" {
		t.Fatalf("issuer startup failure: %v", err)
	}
}
