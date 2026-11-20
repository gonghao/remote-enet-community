package app

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/testutil/fakezgw"
	"remote-enet/internal/ticket"
)

type centerRoundTrip func(*http.Request) (*http.Response, error)

func (rt centerRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

func TestCenterReception(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "active"
		if reject {
			name = "rejected"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			fc, cc, node, server := startCenterHub(t, ctx)
			committing, release := make(chan struct{}, 1), make(chan struct{})
			hc := *fc.HTTPClient()
			orig := hc.Transport
			hc.Transport = centerRoundTrip(func(r *http.Request) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/established") {
					select {
					case committing <- struct{}{}:
					default:
					}
					select {
					case <-release:
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				return orig.RoundTrip(r)
			})
			clientHTTP := *fc.HTTPClient()
			observed, allowActive := make(chan struct{}, 1), make(chan struct{})
			clientHTTP.Transport = centerRoundTrip(func(r *http.Request) (*http.Response, error) {
				resp, err := orig.RoundTrip(r)
				if !reject && err == nil && r.URL.Query().Has("since_version") {
					select {
					case observed <- struct{}{}:
					default:
					}
					select {
					case <-allowActive:
					case <-r.Context().Done():
						resp.Body.Close()
						return nil, r.Context().Err()
					}
				}
				return resp, err
			})
			op, cl := NewWithCenterHTTP(&hc), NewWithCenterHTTP(&clientHTTP)
			nic, gw := gatewayState(t)
			zgw, err := fakezgw.Start(ctx, gw.Address, fakezgw.Profile{Ident: gw.Identification}, fakezgw.WithPorts(0, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			defer zgw.Close()
			diag, ctrl, _ := zgw.Addr()
			cfg := OperatorConfig{StateDir: t.TempDir(), Center: &CenterConfig{Center: cc, Node: node.String()}, Bind: nic.Address, Ports: localPorts(t), RemoteDiagnosticPort: uint16(diag.Port), RemoteControlPort: uint16(ctrl.Port), Logger: quietLogger()}
			account, err := op.CenterAccount(ctx, cfg.StateDir, *cfg.Center)
			if err != nil {
				t.Fatal(err)
			}
			_, enroll := fc.CreateAccount("test", 2, time.Time{})
			if err := account.Login(ctx, enroll, ""); err != nil {
				t.Fatal(err)
			}
			opStates, clStates := op.Subscribe(ctx), cl.Subscribe(ctx)
			opDone, clDone := make(chan error, 1), make(chan error, 1)
			go func() { opDone <- op.RunOperator(ctx, cfg) }()
			snapshotWhere(t, opStates, func(s Snapshot) bool { return s.Center.Phase == "open" })
			code, _ := op.CenterDisplay()
			if len(code) != 8 {
				t.Fatal("missing local invitation")
			}
			n := networkForTest(nic, gw)
			clientCfg := ClientConfig{StateDir: t.TempDir(), Center: &cc, CenterCode: code, NICName: nic.Name, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: n.interfaces, Discover: n.discover}}
			missing := clientCfg
			missing.Hooks.Interfaces = func(context.Context) ([]carside.NIC, error) { return nil, nil }
			if err := NewWithCenterHTTP(fc.HTTPClient()).RunClient(ctx, missing); !errors.Is(err, PairingErrorKind("nic_missing")) || fc.Calls(0, "/v1/redeem") != 0 {
				t.Fatalf("missing NIC: %v", err)
			}
			go func() { clDone <- cl.RunClient(ctx, clientCfg) }()
			a := snapshotWhere(t, opStates, func(s Snapshot) bool { return s.Center.SASID != 0 })
			b := snapshotWhere(t, clStates, func(s Snapshot) bool { return s.Center.SASID != 0 })
			_, pa := op.CenterDisplay()
			_, pb := cl.CenterDisplay()
			if pa.Code != pb.Code || len(pa.Code) != 6 || a.Center.SASID != pa.ID || b.Center.SASID != pb.ID {
				t.Fatal("inconsistent local prompts")
			}
			assertHeld := func() {
				t.Helper()
				portsReleased(t, Snapshot{Diagnostic: netip.AddrPortFrom(nic.Address, uint16(cfg.Ports.Diagnostic)), Control: netip.AddrPortFrom(nic.Address, uint16(cfg.Ports.Control)), Discovery: netip.AddrPortFrom(nic.Address, uint16(cfg.Ports.Discovery))})
				if op.Snapshot().Generation != 0 || cl.Snapshot().Generation != 0 {
					t.Fatal("data plane started before commit")
				}
			}
			assertHeld()
			if err := op.AnswerSAS(ctx, pb.ID, true); !errors.Is(err, ErrSASExpired) {
				t.Fatal(err)
			}
			if !reject {
				server.CloseAll()
				snapshotWhere(t, clStates, func(s Snapshot) bool { return s.Center.Phase == "retry" })
				if err := cl.AnswerSAS(ctx, pb.ID, true); !errors.Is(err, ErrSASExpired) {
					t.Fatal("interrupted prompt still live", err)
				}
				if err := cl.RetryPairing(ctx); err != nil {
					t.Fatal(err)
				}
				snapshotWhere(t, opStates, func(s Snapshot) bool { return s.Center.SASID != 0 && s.Center.SASID != pa.ID })
				snapshotWhere(t, clStates, func(s Snapshot) bool {
					return s.Center.SASID != 0 && s.Center.SASID != pb.ID && s.Center.AttemptsLeft == 1
				})
				_, pa = op.CenterDisplay()
				_, pb = cl.CenterDisplay()
				if pa.Code != pb.Code {
					t.Fatal("retried SAS differs")
				}
			}
			if err := op.AnswerSAS(ctx, pa.ID, !reject); err != nil {
				t.Fatal(err)
			}
			if err := op.AnswerSAS(ctx, pa.ID, true); !errors.Is(err, ErrSASExpired) {
				t.Fatal(err)
			}
			reason := centerflow.Rejected
			if !reject {
				if err := cl.AnswerSAS(ctx, pb.ID, true); err != nil {
					t.Fatal(err)
				}
				select {
				case <-committing:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				assertHeld()
				close(release)
				select {
				case <-observed:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				if cl.Snapshot().Generation != 0 || cl.Snapshot().Phase == PhaseConnected {
					t.Fatal("client forwarded before observing active")
				}
				close(allowActive)
				a = connectedGeneration(t, opStates, 1)
				connectedGeneration(t, clStates, 1)
				sendDiagnostic(t, a.Diagnostic).Close()
				server.CloseAll()
				a = connectedGeneration(t, opStates, 2)
				sendDiagnostic(t, a.Diagnostic).Close()
				if err := cl.EndReception(ctx); err != nil {
					t.Fatal(err)
				}
				reason = centerflow.ClientEnded
			}
			for _, ch := range []chan error{clDone, opDone} {
				select {
				case err := <-ch:
					if err != nil && !errors.Is(err, context.Canceled) && centerflow.ReasonOf(err) != reason {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			}
			for _, r := range []*Runtime{op, cl} {
				if got := r.Snapshot().Center; got.Phase != "ended" || got.Reason != reason || got.SASID != 0 {
					t.Fatalf("final state %+v", got)
				}
			}
			if !reject {
				portsReleased(t, a)
			}
		})
	}
}

func startCenterHub(t *testing.T, parent context.Context) (*fakecenter.Center, settings.Center, center.NodeID, *hub.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	t.Cleanup(cancel)
	fc := fakecenter.New(t, fakecenter.Options{})
	cc := settings.Center{ID: "test-center", API: fc.Entries()}
	local, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var node center.NodeID
	if err := node.UnmarshalText([]byte(fc.RegisterNode(fakecenter.NodeConfig{FP: local.Fingerprint(), Addr: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Capacity: 5}))); err != nil {
		t.Fatal(err)
	}
	api, err := center.NewClient(center.ClientOptions{Center: cc, Role: "hub", Node: node, NodeKey: local.Key, HTTP: fc.HTTPClient()})
	if err != nil {
		t.Fatal(err)
	}
	activation, err := api.Activate(ctx, center.NewIdempotencyKey(), center.Activation{Version: "1.0.0", Kids: []center.KeyID{center.KeyID(ticket.KidOf(fc.TicketKey()))}})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := hubcenter.NewPolicy(hubcenter.Config{Version: 1, Mode: "center", IdentityDir: "test", Listen: ln.Addr().String(), Center: cc, NodeID: node, TicketKeys: []ed25519.PublicKey{fc.TicketKey()}, SetupLimits: hubauth.SetupLimits{Concurrent: 64, PerSecond: 64, Burst: 64, TimeoutS: 10}}, local.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	policy.SetGeneration(activation.Gen)
	server := hub.NewServer(hub.Options{Policy: policy, Local: local})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return fc, cc, node, server
}

func TestCenterModesExclusive(t *testing.T) {
	cc := settings.Center{ID: "test-center", API: []string{"https://127.0.0.1"}}
	for _, c := range []ClientConfig{
		{Center: &cc, Invite: "invitation"},
		{Center: &cc, InviteFile: "file"},
		{Center: &cc, DirectPair: &DirectPair{}},
		{Center: &cc, Peer: identity.Fingerprint{1}},
	} {
		c.StateDir = t.TempDir()
		if err := c.defaults(); err == nil {
			t.Fatal("accepted multiple pairing sources")
		}
	}
	for _, op := range []OperatorConfig{{Listen: "127.0.0.1:47000"}, {Hub: &HubConfig{}}, {ShareAddress: "localhost:47000"}} {
		op.StateDir = t.TempDir()
		op.Bind = netip.MustParseAddr("127.0.0.1")
		op.Center = &CenterConfig{Center: cc}
		if err := op.validate(); err == nil {
			t.Fatal("accepted multiple operator modes")
		}
	}
	r := New()
	err := r.RunClient(t.Context(), ClientConfig{StateDir: t.TempDir(), CenterCode: "12345678", Logger: quietLogger()})
	if !errors.Is(err, PairingErrorKind("center_unconfigured")) || r.Snapshot().Center.Reason != centerflow.CenterUnconfigured {
		t.Fatal(err)
	}
}
