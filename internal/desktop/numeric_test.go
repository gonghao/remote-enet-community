package desktop

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/diagnostics"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
)

func TestSavedPeerIdentityMismatchMessage(t *testing.T) {
	f := newRuntime()
	c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	const kind = app.PairingErrorKind("identity_mismatch")
	const message = "Device identity mismatch"
	f.emit(app.Snapshot{Role: session.RoleClient, Phase: app.PhaseBackoff, LastError: kind.Error()})
	backoff := stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseBackoff })
	if backoff.Runtime.LastError != message {
		t.Fatalf("backoff message: %q", backoff.Runtime.LastError)
	}
	f.finish <- fmt.Errorf("test failure: %w", kind)
	stopped := stateWhere(t, c, func(s State) bool { return !s.Running })
	if stopped.Error != message || stopped.Runtime.LastError != message || stopped.PairingHint != "" {
		t.Fatalf("stop message: %q, runtime message: %q, hint: %q", stopped.Error, stopped.Runtime.LastError, stopped.PairingHint)
	}
}

type discoveredClient struct{ *app.Runtime }

func (r discoveredClient) RunClient(ctx context.Context, cfg app.ClientConfig) error {
	nic := carside.NIC{Name: "fixture", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}
	cfg.NICName, cfg.NICAddr = nic.Name, nic.Address
	cfg.Hooks.Interfaces = func(context.Context) ([]carside.NIC, error) { return []carside.NIC{nic}, nil }
	cfg.Hooks.Discover = func(context.Context, carside.DiscoverOptions) ([]carside.Gateway, error) {
		return []carside.Gateway{{Address: netip.MustParseAddr("127.0.0.2")}}, nil
	}
	return r.Runtime.RunClient(ctx, cfg)
}

func TestDigitalPairResultAcceptedLeavesSavedSettingsUntouched(t *testing.T) {
	local, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	embedded := hub.NewEmbedded()
	_, acceptor, err := hubtransport.New(hubtransport.Options{
		Local: local, Embedded: embedded,
		Wrap: func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
			return secure.WrapStream(ctx, conn, secure.Opts{Local: local, IsClient: isClient, Expect: expect, Path: path})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer acceptor.Close()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	server := hub.NewServer(hub.Options{Local: local, Embedded: embedded})
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, listener) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("fake operator listener: %v", err)
		}
	}()

	cfg := clientSettings()
	saves := make(chan settings.Config, 1)
	c := testController(t, cfg, func(d *dependencies) {
		d.newRuntime = func() appRuntime { return discoveredClient{app.New()} }
		d.save = func(_ string, cfg settings.Config) error { saves <- cfg; return nil }
	})
	if err := c.StartClient(ctx, listener.Addr().String()+" 00001234"); err != nil {
		t.Fatal(err)
	}
	ts, err := acceptor.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close(nil)
	st, err := ts.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := proto.Read(st, proto.MaxFrameLen); err != nil {
		t.Fatal(err)
	}
	if err := proto.Write(st, proto.Hello{Role: "operator", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}); err != nil {
		t.Fatal(err)
	}
	if msg, err := proto.ReadPair(st); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(proto.PairShareA); !ok {
		t.Fatalf("expected client share, got %T", msg)
	}
	if err := proto.Write(st, proto.PairResult{Accepted: true}); err != nil {
		t.Fatal(err)
	}
	stopped := stateWhere(t, c, func(s State) bool { return !s.Running })
	if stopped.Error != "Pairing interrupted" || stopped.Runtime.Generation != 0 || stopped.Config.Client.Peer != cfg.Client.Peer {
		t.Fatalf("fake acceptance affected settings or connection state: %q, generation %d, peer %q", stopped.Error, stopped.Runtime.Generation, stopped.Config.Client.Peer)
	}
	select {
	case <-saves:
		t.Fatal("fake acceptance wrote settings")
	default:
	}
}

func TestNumericCodeRoleAndIdleAdmission(t *testing.T) {
	c := testController(t, clientSettings(), func(*dependencies) {})
	if _, err := c.IssueNumericCode(t.Context()); !errors.Is(err, ErrRole) {
		t.Fatal("client issued an operator code")
	}
	if _, err := c.CopyNumericCode(t.Context(), 1); !errors.Is(err, ErrRole) {
		t.Fatal("client copied an operator code")
	}
	if err := c.RevokeNumericCode(t.Context(), 1); !errors.Is(err, ErrRole) {
		t.Fatal("client revoked an operator code")
	}

	operator := testController(t, operatorSettings(), func(*dependencies) {})
	if _, err := operator.IssueNumericCode(t.Context()); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatal("idle operator issued a code")
	}
}

func TestNumericCodeLifecycleAndSecrets(t *testing.T) {
	f := newRuntime()
	cfg := operatorSettings()
	cfg.Operator.Endpoint = "OPERATOR.example:047005"
	c := testController(t, cfg, func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	start := func(expected string) {
		t.Helper()
		if err := c.StartOperator(t.Context()); err != nil {
			t.Fatal(err)
		}
		call := await(t, f.started)
		if call.operator.ShareAddress != expected || call.operator.Listen != cfg.Operator.Listen {
			t.Fatal("operator did not separate the canonical share address from listen")
		}
		if _, err := c.IssueNumericCode(t.Context()); !errors.Is(err, cpace.ReasonBusy) {
			t.Fatal("operator issued a code before listening")
		}
		f.emit(app.Snapshot{Role: session.RoleOperator, Phase: app.PhaseListening})
		stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseListening })
	}
	start("operator.example:47005")
	states := c.Subscribe(t.Context())
	issue := func() pairing.Display {
		t.Helper()
		d, err := c.IssueNumericCode(t.Context())
		if err != nil || len(d.Code) != 8 || d.Generation == 0 || d.AttemptsLeft != 5 {
			t.Fatal("operator failed to issue a live code")
		}
		s := await(t, states)
		if s.NumericCode != d.Status || s.Invitation != "" {
			t.Fatal("incorrect numeric code projection")
		}
		data, err := diagnostics.Build(diagnostics.Input{
			Version: "1.0.0", Role: s.Config.Role, Runtime: s.Runtime, Running: s.Running,
			Permission: diagnostics.Permission{Platform: runtime.GOOS},
		})
		if err != nil {
			t.Fatal(err)
		}
		assertNoNumericSecret(t, d.Code, fmt.Sprint(s)+string(data))
		return d
	}
	old := issue()
	latest := issue()
	if old.Generation == latest.Generation {
		t.Fatal("regeneration reused the previous generation")
	}
	if _, err := c.CopyNumericCode(t.Context(), old.Generation); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatal("stale copy succeeded")
	}
	if err := c.RevokeNumericCode(t.Context(), old.Generation); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatal("stale revoke succeeded")
	}
	clipboard, err := c.CopyNumericCode(t.Context(), latest.Generation)
	if err != nil || clipboard != latest.Address+" "+latest.Code[:4]+" "+latest.Code[4:] {
		t.Fatal("explicit copy did not preserve the share address and grouped code")
	}
	if err := c.RevokeNumericCode(t.Context(), latest.Generation); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().NumericCode.Reason != cpace.ReasonUnavailable {
		t.Fatal("revocation did not publish a terminal status")
	}
	latest = issue()
	if err := c.Stop(t.Context()); err != nil || c.Snapshot().NumericCode != (pairing.Status{}) {
		t.Fatal("stop retained numeric code state")
	}
	for _, endpoint := range []string{"0.0.0.0:47000", "[::]:47000"} {
		invalid := cfg
		invalid.Operator.Endpoint = endpoint
		if err := c.Save(t.Context(), invalid); err == nil {
			t.Fatal("an unspecified address was accepted as a share address")
		}
	}
	cfg.Operator.Endpoint = "new.example:47006"
	if err := c.Save(t.Context(), cfg); err != nil || c.Snapshot().NumericCode != (pairing.Status{}) {
		t.Fatal("changing share address retained numeric code state")
	}
	f = newRuntime()
	start("new.example:47006")
	replacement := issue()
	if replacement.Generation <= latest.Generation {
		t.Fatal("restart reused old desktop generation numbers")
	}
	if _, err := c.CopyNumericCode(t.Context(), old.Generation); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatal("stale copy reached a new listener with a recycled runtime generation")
	}
	if err := c.RevokeNumericCode(t.Context(), old.Generation); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatal("stale revoke reached a new listener with a recycled runtime generation")
	}
}

func TestStopInvalidatesNumericIssueInFlight(t *testing.T) {
	f := newRuntime()
	entered := make(chan context.Context, 1)
	f.issue = func(ctx context.Context) (pairing.Display, error) {
		entered <- ctx
		<-ctx.Done()
		return pairing.Display{Code: "00001234"}, nil
	}
	c := testController(t, operatorSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	f.emit(app.Snapshot{Role: session.RoleOperator, Phase: app.PhaseListening})
	done := make(chan error, 1)
	go func() {
		display, err := c.IssueNumericCode(t.Context())
		if display != (pairing.Display{}) {
			t.Error("late code escaped after Stop")
		}
		done <- err
	}()
	await(t, entered)
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("late issue escaped the Stop epoch")
	}
	assertNoNumericSecret(t, "00001234", fmt.Sprint(c.Snapshot()))
}

func assertNoNumericSecret(t *testing.T, code, value string) {
	t.Helper()
	if strings.Contains(value, code) || strings.Contains(value, code[:4]+" "+code[4:]) {
		t.Fatal("numeric secret escaped into state or diagnostics")
	}
}
