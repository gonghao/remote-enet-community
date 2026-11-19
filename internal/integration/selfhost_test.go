package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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
	if st := m.Status(); st.AttemptsLeft != d.AttemptsLeft-1 || st.Redeemed {
		t.Fatalf("unexpected failure budget: remaining=%d redeemed=%v", st.AttemptsLeft, st.Redeemed)
	}
	// Both identity creation and read-only peer lookups leave a new trust store
	// unwritten. A file here would also catch a peer saved under the wrong ID.
	if _, err := os.Stat(filepath.Join(f.client.StateDir, "trust.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed pairing did not leave client store unwritten", err)
	}
}

func TestSelfhostTransparentForwarding(t *testing.T) {
	f := newSelfhost(t)
	op := f.operatorReady(t)
	code := f.issue(t, op)
	f.waiting(t, op)
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	deadline, _ := f.ctx.Deadline()
	if err := ln.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	f.client.DirectPair.Address = ln.Addr().String()
	client, stop := f.clientRun(t, f.client)
	front, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer front.Close()
	back, err := (&net.Dialer{}).DialContext(f.ctx, "tcp4", f.cfg.Listen)
	if err != nil {
		t.Fatal(err)
	}
	defer back.Close()
	// This single TCP hop has no TLS identity. The outer TLS session spans
	// the hop, so the Client must authenticate the real Hub at a new address.
	var copies sync.WaitGroup
	copies.Go(func() { _, _ = io.Copy(back, front) })
	copies.Go(func() { _, _ = io.Copy(front, back) })
	defer func() {
		stop()
		front.Close()
		back.Close()
		copies.Wait()
	}()
	waitPhase(t, f.ctx, client, app.PhaseConnected)
	waitPhase(t, f.ctx, op, app.PhaseConnected)
	st := op.NumericCodeStatus()
	if st.Generation != code.Generation || !st.Redeemed {
		t.Fatal("pairing generation was not redeemed")
	}
	cs, osnap := client.Snapshot(), op.Snapshot()
	ep := f.saved(t, osnap.Device)
	if ep.Addr != ln.Addr().String() || ep.Hub != f.host.Fingerprint() || cs.Path.Hub != ep.Hub {
		t.Fatal("incorrect observed or persisted outer endpoint")
	}
	_, claims, err := ticket.Inspect([]byte(ep.Ticket))
	if err != nil || claims.Bearer || claims.Hub != ep.Hub || claims.Device != cs.Device || claims.Peer != osnap.Device {
		t.Fatal("incorrect saved device ticket", err)
	}
	trusted, err := numericStore(t, f.operator.StateDir).Trusted(f.ctx, cs.Device)
	if err != nil || !trusted {
		t.Fatal("missing operator trust", err)
	}
}

// impostorPolicy offers the impostor Hub's own pairing entry to anyone and
// routes it to the relay device R, as a hostile Hub operator would.
type impostorPolicy struct {
	hub, relay identity.Fingerprint
	until      time.Time
	mu         sync.Mutex
	entered    []identity.Fingerprint
}

func (p *impostorPolicy) Admit(dev identity.Fingerprint, req hub.Request, _ time.Time) (hub.Grant, error) {
	switch {
	case req.Role == hub.RoleDial && req.Target == p.hub && len(req.Token) == 0:
		p.mu.Lock()
		p.entered = append(p.entered, dev)
		p.mu.Unlock()
		return hub.Grant{PairingUntil: p.until}, nil
	case req.Role == hub.RoleWait && dev == p.relay && req.Target.IsZero():
		return hub.Grant{PairingUntil: p.until}, nil
	}
	return hub.Grant{}, errors.New("impostor: route not offered")
}

func (p *impostorPolicy) Valid(hub.Grant, identity.Fingerprint) bool { return true }

func (p *impostorPolicy) PairingOperator() identity.Fingerprint { return p.relay }

func (p *impostorPolicy) dials() []identity.Fingerprint {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]identity.Fingerprint(nil), p.entered...)
}

// relayToPairingEntry keeps the impostor wait silent until the Client's first
// inner bytes arrive, then enters the real Hub's pairing entry as the relay
// device and copies opaque bytes both ways. It returns the real Hub admission
// result; copy errors are the expected end of the relay.
func relayToPairingEntry(ctx context.Context, wait net.Conn, addr string, relay *identity.Local, hubFP identity.Fingerprint) error {
	defer wait.Close()
	stop := context.AfterFunc(ctx, func() { _ = wait.Close() })
	defer stop()
	first := make([]byte, 32*1024)
	n, err := wait.Read(first)
	if err != nil {
		return fmt.Errorf("read first inner bytes: %w", err)
	}
	dial, err := hub.Connect(ctx, addr, relay, hubFP, hub.Request{Role: hub.RoleDial, Target: hubFP})
	if err != nil {
		return fmt.Errorf("enter real pairing entry: %w", err)
	}
	defer dial.Close()
	stopDial := context.AfterFunc(ctx, func() { _ = dial.Close() })
	defer stopDial()
	if _, err := dial.Write(first[:n]); err != nil {
		return nil
	}
	var copies sync.WaitGroup
	copies.Go(func() {
		_, _ = io.Copy(dial, wait)
		_ = dial.Close()
	})
	_, _ = io.Copy(wait, dial)
	_ = wait.Close()
	copies.Wait()
	return nil
}

// sessionEnds forwards the error of each operator session end record.
type sessionEnds struct {
	slog.Handler
	errs chan<- error
}

func (h sessionEnds) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == "operator session ended" {
		r.Attrs(func(a slog.Attr) bool {
			if err, ok := a.Value.Any().(error); ok && a.Key == "error" {
				select {
				case h.errs <- err:
				default:
				}
			}
			return true
		})
	}
	return h.Handler.Handle(ctx, r)
}

func (h sessionEnds) WithAttrs(attrs []slog.Attr) slog.Handler {
	return sessionEnds{h.Handler.WithAttrs(attrs), h.errs}
}

func (h sessionEnds) WithGroup(name string) slog.Handler {
	return sessionEnds{h.Handler.WithGroup(name), h.errs}
}

// An impostor Hub H' terminates the Client's outer TLS and forwards the inner
// bytes through the real Hub's pairing entry with its own relay device. Unlike
// TestSelfhostTransparentForwarding, the outer identity seen by the Client is
// H', so the hub_fp bound into the PAKE differs and both ends must refuse.
func TestSelfhostImpostorHubFailsPairing(t *testing.T) {
	f := newSelfhost(t)
	ended := make(chan error, 4)
	f.operator.Logger = slog.New(sessionEnds{f.operator.Logger.Handler(), ended})
	op := f.operatorReady(t)
	d := f.issue(t, op)
	f.waiting(t, op)
	realHub, operator := f.host.Fingerprint(), op.Snapshot().Device
	impostor, relay := localIdentity(t), localIdentity(t)
	policy := &impostorPolicy{hub: impostor.Fingerprint(), relay: relay.Fingerprint(), until: d.ExpiresAt}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := hub.NewServer(hub.Options{Local: impostor, Policy: policy, Logger: f.operator.Logger})
	runRuntime(t, f.ctx, func(ctx context.Context) error { return srv.Serve(ctx, ln) })
	wait, err := hub.Connect(f.ctx, ln.Addr().String(), relay, impostor.Fingerprint(), hub.Request{Role: hub.RoleWait})
	if err != nil {
		t.Fatal(err)
	}
	relayed := make(chan error, 1)
	var relaying sync.WaitGroup
	relaying.Go(func() { relayed <- relayToPairingEntry(f.ctx, wait, f.cfg.Listen, relay, realHub) })
	defer func() {
		_ = wait.Close()
		relaying.Wait()
	}()

	f.client.DirectPair.Address = ln.Addr().String()
	c := f.fail(t, "confirmation")
	device := c.Snapshot().Device
	select {
	case err := <-ended:
		if !errors.Is(err, cpace.ReasonConfirmation) {
			t.Fatal("operator confirmation", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	select {
	case err := <-relayed:
		if err != nil {
			t.Fatal("relay did not reach the real operator", err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	// The Client sent target = its verified outer fingerprint, so the impostor
	// admitting its own pairing entry proves the Client saw H'. The real Hub
	// routes its pairing entry only to the real Operator, whose code budget
	// below proves it ran the PAKE: the inner peer O differs from H', so the
	// Client bound pair-hub/1 to H' while the Operator bound its configured H.
	if dials := policy.dials(); len(dials) != 1 || dials[0] != device {
		t.Fatal("client did not enter the impostor pairing entry", dials)
	}
	if operator == impostor.Fingerprint() || realHub == impostor.Fingerprint() || f.operator.Hub.FP != realHub {
		t.Fatal("fixture identities overlap")
	}
	if st := op.NumericCodeStatus(); st.Generation != d.Generation || st.AttemptsLeft != d.AttemptsLeft-1 || st.Redeemed {
		t.Fatalf("unexpected failure budget: generation=%d remaining=%d redeemed=%v", st.Generation, st.AttemptsLeft, st.Redeemed)
	}
	trusted, err := numericStore(t, f.operator.StateDir).Trusted(f.ctx, device)
	if err != nil || trusted {
		t.Fatal("unexpected operator trust", err)
	}
	f.absent(t, operator)
	f.absent(t, impostor.Fingerprint())
	if _, err := os.Stat(filepath.Join(f.client.StateDir, "trust.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed pairing did not leave client store unwritten", err)
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
