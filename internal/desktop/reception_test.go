package desktop

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/center"
	"remote-enet/internal/centercode"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/checkup"
	"remote-enet/internal/diagnostics"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/ticket"
)

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

func TestControllerCenterReception(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	fc, cc, node, server := startCenterHub(t, ctx)
	cfg := loopbackOperatorSettings(t)
	cfg.Operator.Mode, cfg.Center = "center", cc
	op := realController(t, cfg, func() appRuntime { return app.NewWithCenterHTTP(fc.HTTPClient()) })
	clcfg := clientSettings()
	clcfg.Center = cc
	cl := realController(t, clcfg, func() appRuntime { return discoveredClient{app.NewWithCenterHTTP(fc.HTTPClient())} })
	if err := op.EndReception(ctx); !errors.Is(err, app.ErrNotConnected) {
		t.Fatal(err)
	}
	if err := cl.RefreshCenter(ctx); !errors.Is(err, ErrRole) {
		t.Fatal(err)
	}
	if err := op.StartReception(ctx, node.String()); !errors.Is(err, centerflow.Unauthenticated) {
		t.Fatal(err)
	}
	transport := http.DefaultTransport
	http.DefaultTransport = fc.HTTPClient().Transport
	defer func() { http.DefaultTransport = transport }()
	if err := op.CheckConnection(ctx, ""); err != nil {
		t.Fatal(err)
	}
	hasFinding := func(c *Controller, code string) bool {
		for _, f := range c.Snapshot().Checkup.Findings {
			if f.Code == code {
				return true
			}
		}
		return false
	}
	if !hasFinding(op, "unauthenticated") {
		t.Fatal("fresh operator reported a login")
	}
	if err := op.CenterLogin(ctx, "short", ""); !errors.Is(err, centercode.ErrCodeLength) {
		t.Fatal(err)
	}
	_, enroll := fc.CreateAccount("fixture", 2, time.Time{})
	if err := op.CenterLogin(ctx, enroll, "bench"); err != nil {
		t.Fatal(err)
	}
	if err := op.CheckConnection(ctx, ""); err != nil || !hasFinding(op, "signed_in") {
		t.Fatal("self-check did not verify enrollment", err)
	}
	view := op.Snapshot().Center
	if !view.LoggedIn || view.Account.Quota != 2 || len(view.Devices) != 1 || !view.Devices[0].Current || len(view.Nodes) != 1 || view.Nodes[0].Latency < 0 {
		t.Fatal("account projection incomplete")
	}
	if err := op.CenterLogin(ctx, enroll, ""); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := op.RevokeCenterDevice(ctx, "unknown"); !errors.Is(err, centerflow.NotFound) {
		t.Fatal(err)
	}
	other := realController(t, cfg, func() appRuntime { return app.NewWithCenterHTTP(fc.HTTPClient()) })
	if err := other.CenterLogin(ctx, enroll, "spare"); err != nil {
		t.Fatal(err)
	}
	if err := op.RefreshCenter(ctx); err != nil {
		t.Fatal(err)
	}
	devices := op.Snapshot().Center.Devices
	if len(devices) != 2 {
		t.Fatal("missing enrolled device")
	}
	for _, device := range devices {
		if !device.Current {
			if err := op.RevokeCenterDevice(ctx, device.DeviceFP.String()); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !errors.Is(other.RefreshCenter(ctx), centerflow.Unauthenticated) || other.Snapshot().Center.LoggedIn {
		t.Fatal("revoked device retained account access")
	}
	if _, err := op.IssueInvite(ctx); !errors.Is(err, ErrRole) {
		t.Fatal("raw invitation available in center mode")
	}

	if err := op.StartReception(ctx, "unknown"); !errors.Is(err, centerflow.NodeUnavailable) {
		t.Fatal(err)
	}
	if err := op.StartReception(ctx, node.String()); err != nil {
		t.Fatal(err)
	}
	stateWhere(t, op, func(s State) bool { return s.Center.Phase == "open" })
	if err := op.CenterLogout(ctx); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	code, _ := op.CenterDisplay()
	if len(code) != 8 {
		t.Fatal("missing invitation")
	}
	if err := cl.CheckConnection(ctx, code); err != nil || !hasFinding(cl, "center_verified") || fc.Calls(0, "/v1/redeem") != 0 {
		t.Fatal("self-check consumed code or missed meta", err)
	}
	if err := cl.StartClient(ctx, code); err != nil {
		t.Fatal(err)
	}
	a := stateWhere(t, op, func(s State) bool { return s.Center.SASID != 0 })
	b := stateWhere(t, cl, func(s State) bool { return s.Center.SASID != 0 })
	_, prompt := cl.CenterDisplay()
	if code, p := op.CenterDisplay(); code != "" || p.Code != "" || len(prompt.Code) != 6 {
		t.Fatal("incorrect local secret display")
	}
	if !errors.Is(op.AnswerSAS(ctx, a.Center.SASID, true), ErrSASInputRequired) {
		t.Fatal("accepted unverified input")
	}
	server.CloseAll()
	stateWhere(t, cl, func(s State) bool { return s.Center.Phase == "retry" })
	if !errors.Is(cl.AnswerSAS(ctx, b.Center.SASID, true), app.ErrSASExpired) {
		t.Fatal("stale prompt accepted")
	}
	if err := cl.RetryPairing(ctx); err != nil {
		t.Fatal(err)
	}
	a = stateWhere(t, op, func(s State) bool { return s.Center.SASID != 0 && s.Center.SASID != a.Center.SASID })
	b = stateWhere(t, cl, func(s State) bool { return s.Center.SASID != 0 && s.Center.SASID != b.Center.SASID })
	_, prompt = cl.CenterDisplay()
	if op.MatchSAS(a.Center.SASID, "bad") || !op.MatchSAS(a.Center.SASID, prompt.Code) {
		t.Fatal("operator comparison failed")
	}
	const ticket = "private-ticket-canary"
	op.mu.Lock()
	g := op.run
	op.mu.Unlock()
	slog.New(runtimeLogHandler{controller: op, run: g}).Error("operator session ended", "error", fmt.Errorf("%s %s %s %s", enroll, code, prompt.Code, ticket))
	for _, c := range []*Controller{op, cl} {
		s := await(t, c.Subscribe(ctx))
		data, err := diagnostics.Build(diagnostics.Input{Version: "1.0.0", Role: s.Config.Role, Runtime: s.Runtime, Permission: diagnostics.Permission{Platform: runtime.GOOS}})
		if err != nil {
			t.Fatal(err)
		}
		report, err := checkup.JSON(s.Checkup)
		if err != nil {
			t.Fatal(err)
		}
		// Check textual fields independently of random timer values or fingerprints.
		projection, err := json.Marshal(s.Center)
		if err != nil {
			t.Fatal(err)
		}
		output := string(projection) + strings.Join(s.Events, " ") + s.Error + s.Notice + s.Runtime.LastError + s.Invitation + string(data) + string(report)
		for _, secret := range []string{enroll, code, prompt.Code, ticket} {
			if strings.Contains(output, secret) {
				t.Fatal("secret retained by a public output")
			}
		}
	}
	if err := op.AnswerSAS(ctx, a.Center.SASID, true); err != nil {
		t.Fatal(err)
	}
	if err := cl.AnswerSAS(ctx, b.Center.SASID, true); err != nil {
		t.Fatal(err)
	}
	stateWhere(t, op, func(s State) bool { return s.Center.Phase == "active" })
	if err := op.EndReception(ctx); err != nil {
		t.Fatal(err)
	}
	stateWhere(t, op, func(s State) bool { return !s.Running && s.Center.Reason == centerflow.OperatorEnded })
	if err := op.CenterLogout(ctx); err != nil {
		t.Fatal(err)
	}
	if op.Snapshot().Center.LoggedIn || centerflow.ReasonOf(op.RefreshCenter(ctx)) != centerflow.Unauthenticated {
		t.Fatal("local device remained authorized")
	}
}
