package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/checkup"
	"remote-enet/internal/diagnostics"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/ticket"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
)

type selfhost struct {
	*numericFixture
	host        *identity.Local
	cfg         hubauth.Config
	server      *hub.Server
	hubLogs     cliLog
	stopHub     func()
	clockOffset atomic.Int64
}

func newSelfhost(t *testing.T) *selfhost {
	t.Helper()
	f := &selfhost{numericFixture: newNumericFixture(t), host: localIdentity(t)}
	entry, err := app.IssuerEntry(f.ctx, f.operator.StateDir, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	var issuer hubauth.Issuer
	if err = json.Unmarshal([]byte(entry), &issuer); err != nil {
		t.Fatal(err)
	}
	f.cfg = hubauth.Config{
		Version: 1, Mode: "selfhost", IdentityDir: t.TempDir(),
		Issuers: []hubauth.Issuer{issuer}, PairingOperator: issuer.Operator,
		ClockSkewS: 5, TicketMaxTTLS: 2678400, BootstrapMaxTTLS: 86400,
		PairingWindowMaxS: 900,
		SetupLimits:       hubauth.SetupLimits{Concurrent: 64, PerSecond: 32, Burst: 64, TimeoutS: 5},
	}
	f.startHub(t, "127.0.0.1:0")
	f.operator.Listen, f.operator.ShareAddress = "", ""
	f.operator.Hub = &app.HubConfig{Addr: f.cfg.Listen, FP: f.host.Fingerprint()}
	return f
}

func (f *selfhost) policy(t *testing.T) *hubauth.Policy {
	t.Helper()
	p, err := hubauth.NewPolicy(f.cfg, f.host.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (f *selfhost) startHub(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	f.cfg.Listen = ln.Addr().String()
	srv := hub.NewServer(hub.Options{
		Local: f.host, Policy: f.policy(t),
		Now:    func() time.Time { return time.Now().Add(time.Duration(f.clockOffset.Load())) },
		Logger: slog.New(slog.NewTextHandler(&f.hubLogs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	f.server = srv
	f.stopHub = runRuntime(t, f.ctx, func(ctx context.Context) error { return srv.Serve(ctx, ln) })
}

func stateWhere(t *testing.T, ctx context.Context, r *app.Runtime, match func(app.Snapshot) bool) app.Snapshot {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for s := range r.Subscribe(ctx) {
		if match(s) {
			return s
		}
	}
	t.Fatalf("state timeout: phase=%s error=%s", r.Snapshot().Phase, r.Snapshot().LastError)
	return app.Snapshot{}
}

func (f *selfhost) waiting(t *testing.T, op *app.Runtime) {
	t.Helper()
	stateWhere(t, f.ctx, op, func(s app.Snapshot) bool { return s.HubWaiting })
}

func (f *selfhost) operatorReady(t *testing.T) *app.Runtime {
	t.Helper()
	op, _ := f.listen(t)
	f.waiting(t, op)
	return op
}

func (f *selfhost) invitation(t *testing.T, ttl time.Duration) string {
	t.Helper()
	code, err := app.IssueInvite(f.ctx, app.InviteConfig{StateDir: f.operator.StateDir, Hub: f.operator.Hub, TTL: ttl, Logger: f.operator.Logger})
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func (f *selfhost) clientRun(t *testing.T, cfg app.ClientConfig) (*app.Runtime, func()) {
	t.Helper()
	r := app.New()
	stop := runRuntime(t, f.ctx, func(ctx context.Context) error { return r.RunClient(ctx, cfg) })
	return r, stop
}

func (f *selfhost) saved(t *testing.T, op identity.Fingerprint) transport.Endpoint {
	t.Helper()
	p, err := numericStore(t, f.client.StateDir).Peer(f.ctx, op)
	if err != nil || len(p.Endpoints) != 1 {
		t.Fatal("missing saved endpoint", err)
	}
	return p.Endpoints[0]
}

func TestSelfhostInviteExpiryAndRenewal(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	f.client.Invite = f.invitation(t, 2*time.Second)
	inv, err := invite.Decode(f.client.Invite)
	if err != nil {
		t.Fatal(err)
	}
	c, stop := f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	claims := func() ticket.Claims {
		ep := f.saved(t, inv.Peer.ID)
		_, cl, err := ticket.Inspect([]byte(ep.Ticket))
		if err != nil || cl.Bearer || cl.Device != c.Snapshot().Device || cl.Peer != inv.Peer.ID || cl.Hub != f.host.Fingerprint() {
			t.Fatal("invalid saved device ticket", err)
		}
		return cl
	}
	first := claims()
	stop()
	f.waiting(t, op)
	select {
	case <-time.After(time.Until(inv.ExpiresAt)):
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	if _, err := invite.Decode(f.client.Invite); err == nil {
		t.Fatal("bootstrap is still valid")
	}
	// Advance only the Hub clock beyond its configured admission grace.
	f.clockOffset.Store(int64(time.Duration(f.cfg.ClockSkewS+1) * time.Second))
	ep := inv.Peer.Endpoints[0]
	conn, err := hub.Connect(f.ctx, ep.Addr, localIdentity(t), ep.Hub, hub.Request{Role: hub.RoleDial, Target: inv.Peer.ID, Token: []byte(ep.Ticket)})
	if err == nil {
		conn.Close()
		t.Fatal("Hub accepted expired bearer")
	}
	if !errors.Is(err, hub.ErrUnauthorized) {
		t.Fatal("unexpected admission result", err)
	}
	f.client.Invite, f.client.Peer = "", inv.Peer.ID
	c, _ = f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	if second := claims(); !second.Expires.After(first.Expires) || second.JTI == first.JTI {
		t.Fatal("ticket was not renewed")
	}
}

func TestSelfhostWindow(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	d := f.issue(t, op)
	f.waiting(t, op)
	wrong := "00000000"
	if wrong == d.Code {
		wrong = "00000001"
	}
	f.client.DirectPair.Code = wrong
	f.fail(t, "confirmation")
	f.waiting(t, op)
	if op.NumericCodeStatus().AttemptsLeft != 4 {
		t.Fatal("wrong code did not consume one attempt")
	}
	f.client.DirectPair.Code = d.Code
	c, stop := f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	stop()
	f.waiting(t, op)
	f.client.StateDir = t.TempDir()
	f.fail(t, "unavailable")
}

func TestSelfhostReloadRevokesClient(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	f.client.Invite = f.invitation(t, time.Minute)
	c := app.New()
	done := make(chan error, 1)
	go func() { done <- c.RunClient(f.ctx, f.client) }()
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	f.cfg.Deny.Devices = []string{c.Snapshot().Device.String()}
	_, pairs := f.server.SetPolicy(f.policy(t))
	if pairs != 1 {
		t.Fatal("reload did not close the pair")
	}
	select {
	case err := <-done:
		if !errors.Is(err, app.HubAuthorizationExpired) || c.Snapshot().Generation != 1 {
			t.Fatal("incorrect terminal result", err)
		}
	case <-f.ctx.Done():
		t.Fatal("client kept retrying")
	}
	f.waiting(t, op)
}

func TestSelfhostUnauthorizedWait(t *testing.T) {
	f := newSelfhost(t)
	f.operator.StateDir = t.TempDir()
	start := time.Now()
	op, _ := f.listen(t)
	s := stateWhere(t, f.ctx, op, func(s app.Snapshot) bool { return s.LastError == string(app.HubWaitUnauthorized) })
	if s.HubWaiting || s.Phase != app.PhaseListening || s.NextRetry.Before(start.Add(30*time.Second)) || s.NextRetry.After(time.Now().Add(30*time.Second)) {
		t.Fatal("incorrect registration state or retry deadline")
	}
	// The loopback run above observes the real Hub refusal. In virtual time,
	// feed the same error into the registration loop and measure both attempts.
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 31*time.Second)
		defer cancel()
		attempts := make(chan time.Time, 2)
		_, accept, err := hubtransport.New(hubtransport.Options{
			Local: f.host, Wrap: secureWrap(f.host),
			Endpoints: []transport.Endpoint{{Kind: "hub", Addr: f.cfg.Listen, Hub: f.host.Fingerprint()}},
			WaitToken: func(context.Context, identity.Fingerprint) ([]byte, error) {
				select {
				case attempts <- time.Now():
				case <-ctx.Done():
				}
				return nil, hub.ErrUnauthorized
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer accept.Close()
		go func() { _, _ = accept.Accept(ctx) }()
		var first time.Time
		for range 2 {
			select {
			case at := <-attempts:
				if !first.IsZero() && at.Sub(first) != 30*time.Second {
					t.Fatalf("registration interval %v", at.Sub(first))
				}
				first = at
			case <-ctx.Done():
				t.Fatal("missing retry")
			}
		}
	})
}

func TestSelfhostExpiredBeforeDial(t *testing.T) {
	f := newSelfhost(t)
	store := numericStore(t, f.client.StateDir)
	id, err := store.LoadIdentity(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	op, err := identity.ParseFingerprint(f.cfg.PairingOperator)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.LoadOrCreateIssuer(filepath.Join(f.operator.StateDir, "issuer.key"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := issuer.Dial(time.Now().Add(-ticket.DialTTL-time.Hour), id.Fingerprint(), op, f.host.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := store.SavePeer(f.ctx, transport.PeerInfo{ID: op, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: ln.Addr().String(), Hub: f.host.Fingerprint(), Ticket: string(raw)}}}); err != nil {
		t.Fatal(err)
	}
	f.client.Peer = op
	f.client.Hooks.Interfaces = func(context.Context) ([]carside.NIC, error) { t.Fatal("network enumeration reached"); return nil, nil }
	f.fail(t, app.HubAuthorizationExpired)
	// A completed TCP handshake is queued even when nobody calls Accept.
	if err := ln.SetDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	conn, err := ln.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("expired ticket opened a socket")
	}
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestSelfhostBusyHandoff(t *testing.T) {
	f := newSelfhost(t)
	f.operatorReady(t)
	a, b := f.client, f.client
	a.Invite, b.Invite = f.invitation(t, time.Minute), f.invitation(t, time.Minute)
	b.StateDir = t.TempDir()
	ca, stop := f.clientRun(t, a)
	waitPhase(t, f.ctx, ca, app.PhaseConnected)
	cb, _ := f.clientRun(t, b)
	stateWhere(t, f.ctx, cb, func(s app.Snapshot) bool { return s.Phase == app.PhaseBackoff && s.LastError == "busy" })
	stop()
	waitPhase(t, f.ctx, cb, app.PhaseConnected)
}

func TestSelfhostRestart(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	f.client.Invite = f.invitation(t, time.Minute)
	c, _ := f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	f.stopHub()
	stateWhere(t, f.ctx, c, func(s app.Snapshot) bool { return s.Phase == app.PhaseBackoff })
	f.startHub(t, f.cfg.Listen)
	for _, r := range []*app.Runtime{c, op} {
		stateWhere(t, f.ctx, r, func(s app.Snapshot) bool { return s.Phase == app.PhaseConnected && s.Generation == 2 })
	}
}

func TestSelfhostNonDesignatedOperator(t *testing.T) {
	f := newSelfhost(t)
	entry, err := app.IssuerEntry(f.ctx, t.TempDir(), "designated")
	if err != nil {
		t.Fatal(err)
	}
	var other hubauth.Issuer
	if err := json.Unmarshal([]byte(entry), &other); err != nil {
		t.Fatal(err)
	}
	f.cfg.Issuers = append(f.cfg.Issuers, other)
	f.cfg.PairingOperator = other.Operator
	f.server.SetPolicy(f.policy(t))
	op := f.operatorReady(t)
	d := f.issue(t, op)
	f.waiting(t, op)
	if _, err := op.NumericCodeDisplay(f.ctx, d.Generation); err == nil || string(op.NumericCodeStatus().Reason) != string(app.HubPairingDisabled) {
		t.Fatal("refused code remains available")
	}
	f.client.DirectPair = nil
	f.client.Invite = f.invitation(t, time.Minute)
	c, _ := f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
}

// The two endpoints share the real inner TLS exporter, peer identities, code
// and hellos. Only the responder's view of the outer Hub identity differs.
func TestSelfhostHubFingerprintBinding(t *testing.T) {
	f := newSelfhost(t)
	store := numericStore(t, f.operator.StateDir)
	id, err := store.LoadIdentity(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	issuer, err := ticket.LoadOrCreateIssuer(filepath.Join(f.operator.StateDir, "issuer.key"))
	if err != nil {
		t.Fatal(err)
	}
	m := pairing.NewManager(pairing.Config{Address: f.cfg.Listen})
	defer m.Close()
	d, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	registered := make(chan error, 1)
	ep := transport.Endpoint{Kind: "hub", Addr: f.cfg.Listen, Hub: f.host.Fingerprint()}
	_, accept, err := hubtransport.New(hubtransport.Options{Local: id, Wrap: secureWrap(id), Endpoints: []transport.Endpoint{ep}, WaitToken: func(ctx context.Context, fp identity.Fingerprint) ([]byte, error) {
		return issuer.Wait(time.Now(), id.Fingerprint(), fp, d.ExpiresAt)
	}, OnRegister: func(_ identity.Fingerprint, _ []byte, err error) {
		select {
		case registered <- err:
		default:
		}
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer accept.Close()
	different := localIdentity(t).Fingerprint()
	done := make(chan error, 1)
	go func() {
		ts, err := accept.Accept(f.ctx)
		if err == nil {
			defer ts.Close(nil)
			_, err = session.Run(f.ctx, ts, session.Options{Role: session.RoleOperator, LocalFP: id.Fingerprint(), HubFP: different, DigitalManager: integrationResponder{m}, Remember: store.Remember, GrantTicket: func(peer identity.Fingerprint) ([]byte, error) {
				return issuer.Dial(time.Now(), peer, id.Fingerprint(), different)
			}})
		}
		done <- err
	}()
	select {
	case err := <-registered:
		if err != nil {
			t.Fatal(err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.client.DirectPair = &app.DirectPair{Address: ep.Addr, Code: d.Code}
	c := f.fail(t, "confirmation")
	select {
	case err := <-done:
		if !errors.Is(err, cpace.ReasonConfirmation) {
			t.Fatal("operator confirmation", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	f.absent(t, id.Fingerprint())
	ok, err := store.Trusted(f.ctx, c.Snapshot().Device)
	if err != nil || ok {
		t.Fatal("unexpected operator trust", err)
	}
}

func TestSelfhostSecretSurfaces(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	d := f.issue(t, op)
	f.waiting(t, op)
	f.client.DirectPair = nil
	f.client.Invite = f.invitation(t, time.Minute)
	inv, err := invite.Decode(f.client.Invite)
	if err != nil {
		t.Fatal(err)
	}
	c, stop := f.clientRun(t, f.client)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	ep := f.saved(t, inv.Peer.ID)
	secrets := []string{d.Code, f.client.Invite, ep.Ticket, inv.Peer.Endpoints[0].Ticket}
	check := func(surface, data string) {
		t.Helper()
		for _, secret := range secrets {
			if secret == "" || strings.Contains(data, secret) {
				t.Fatalf("credential in %s or empty fixture", surface)
			}
		}
	}
	for _, r := range []*app.Runtime{c, op} {
		s := <-r.Subscribe(f.ctx)
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		check("subscription", string(raw))
		s.LastError = strings.Join(secrets, " ")
		diag, err := diagnostics.Build(diagnostics.Input{Version: "dev", Runtime: s, Permission: diagnostics.Permission{Platform: runtime.GOOS}})
		if err != nil {
			t.Fatal(err)
		}
		check("diagnostics", string(diag))
	}
	cfg := settings.Default()
	cfg.Client.Peer = inv.Peer.ID.String()
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		bad := cfg
		bad.Client.Peer = secret
		if err := settings.Save(path, bad); err == nil {
			t.Fatal("settings accepted a credential as a peer")
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	check("settings", string(data))
	report := checkup.Run(f.ctx, checkup.Input{Config: cfg, Runtime: c.Snapshot(), Running: true, Endpoints: []transport.Endpoint{ep}, PeerCode: d.Code})
	data, err = checkup.JSON(report)
	if err != nil || !report.Complete {
		t.Fatal("incomplete checkup", err)
	}
	check("checkup", string(data))
	stop()
	denied := localIdentity(t)
	_, err = hub.Connect(f.ctx, ep.Addr, denied, ep.Hub, hub.Request{Role: hub.RoleDial, Target: inv.Peer.ID, Token: []byte(ep.Ticket)})
	if err == nil {
		t.Fatal("unexpected ticket admission")
	}
	check("application logs", f.logs.snapshot())
	logs := f.hubLogs.snapshot()
	if !strings.Contains(logs, "Hub policy denied request") || !strings.Contains(logs, "device="+denied.Fingerprint().Short()) {
		t.Fatal("missing denial log")
	}
	check("hub logs", logs)
	for _, fp := range []identity.Fingerprint{inv.Peer.ID, c.Snapshot().Device, ep.Hub, denied.Fingerprint()} {
		if strings.Contains(logs, fp.String()) {
			t.Fatal("full identity in hub log")
		}
	}
}
