package integration_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/centerstate"
	"remote-enet/internal/diagnostics"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/testutil/fakezgw"
)

type centerHTTP func(*http.Request) (*http.Response, error)

func (f centerHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type receptionFixture struct {
	*numericFixture
	fc                 *fakecenter.Center
	server             *hub.Server
	op, cl             *app.Runtime
	stopOp, stopCl     func()
	account, code, sas string
	http               *http.Client
	mu                 sync.Mutex
	publicSnapshots    []app.Snapshot
	tickets            []string
	clock              receptionClock
	stopNode           func()
	advanceOnHeartbeat atomic.Bool
	holdPolls          atomic.Pointer[chan struct{}]
}

// The Hub schedules absolute lease deadlines via Now and AfterFunc. A clock
// jump must also fire previously armed timers, including ones armed before
// the test began advancing the center clock.
type receptionTimer struct {
	due  time.Time
	fire func()
	wall *time.Timer
}

type receptionClock struct {
	mu     sync.Mutex
	offset atomic.Int64
	timers map[*receptionTimer]bool
}

func (c *receptionClock) now() time.Time {
	return time.Now().Add(time.Duration(c.offset.Load()))
}

func (c *receptionClock) after(d time.Duration, fire func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &receptionTimer{due: c.now().Add(d), fire: fire}
	if c.timers == nil {
		c.timers = make(map[*receptionTimer]bool)
	}
	c.timers[timer] = true
	timer.wall = time.AfterFunc(d, func() {
		c.mu.Lock()
		pending := c.timers[timer]
		delete(c.timers, timer)
		c.mu.Unlock()
		if pending {
			fire()
		}
	})
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		pending := c.timers[timer]
		delete(c.timers, timer)
		timer.wall.Stop()
		return pending
	}
}

func (c *receptionClock) advance(d time.Duration) {
	c.mu.Lock()
	c.offset.Add(int64(d))
	var due []func()
	for timer := range c.timers {
		if !c.now().Before(timer.due) {
			delete(c.timers, timer)
			timer.wall.Stop()
			due = append(due, timer.fire)
		}
	}
	c.mu.Unlock()
	for _, fire := range due {
		fire()
	}
}

func newReception(t *testing.T, timeout time.Duration) *receptionFixture {
	t.Helper()
	f := &receptionFixture{numericFixture: newNumericFixture(t)}
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	f.ctx = ctx
	t.Cleanup(cancel)
	now := f.clock.now
	f.fc = fakecenter.New(t, fakecenter.Options{Entries: 2, Now: now})
	cc := settings.Center{ID: "test-center", API: f.fc.Entries()}
	f.startNode(t, cc)
	f.http = f.applicationHTTP()
	f.op = app.NewWithCenterHTTP(f.http)
	f.operator.Listen, f.operator.ShareAddress = "", ""
	f.operator.Center = &app.CenterConfig{Center: cc}
	f.client.Center = &cc
	acct, err := f.op.CenterAccount(ctx, f.operator.StateDir, *f.operator.Center)
	if err != nil {
		t.Fatal(err)
	}
	_, f.account = f.fc.CreateAccount("fixture", 2, time.Time{})
	if err := acct.Login(ctx, f.account, ""); err != nil {
		t.Fatal(err)
	}
	for {
		nodes, err := acct.Nodes(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(nodes) == 1 && nodes[0].Latency >= 0 {
			f.operator.Center.Node = nodes[0].NodeID.String()
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	gateways, err := f.client.Hooks.Discover(ctx, carside.DiscoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	gw := gateways[0]
	z, err := fakezgw.Start(ctx, gw.Address, fakezgw.Profile{Ident: gw.Identification, Responders: map[string][]byte{"22f190": {0x62, 0xf1, 0x90}}}, fakezgw.WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { z.Close() })
	diag, ctrl, _ := z.Addr()
	f.operator.RemoteDiagnosticPort, f.operator.RemoteControlPort = uint16(diag.Port), uint16(ctrl.Port)
	return f
}

func (f *receptionFixture) applicationHTTP() *http.Client {
	base := f.fc.HTTPClient().Transport
	hc := *f.fc.HTTPClient()
	hc.Transport = centerHTTP(func(r *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(r)
		if gate := f.holdPolls.Load(); gate != nil && err == nil && r.URL.Query().Has("since_version") {
			select {
			case <-*gate:
			case <-r.Context().Done():
				res.Body.Close()
				return nil, r.Context().Err()
			}
		}
		if err != nil || res.StatusCode != 200 || !strings.HasSuffix(r.URL.Path, "/tickets") {
			return res, err
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		var ticket struct{ Ticket string }
		if err := json.Unmarshal(data, &ticket); err != nil {
			return nil, err
		}
		f.mu.Lock()
		f.tickets = append(f.tickets, ticket.Ticket)
		f.mu.Unlock()
		res.Body = io.NopCloser(bytes.NewReader(data))
		return res, nil
	})
	return &hc
}

func (f *receptionFixture) startNode(t *testing.T, cc settings.Center) {
	t.Helper()
	now := f.clock.now
	host := localIdentity(t)
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	id := f.fc.RegisterNode(fakecenter.NodeConfig{FP: host.Fingerprint(), Addr: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port})
	cfg := hubcenter.Config{
		Version: 1, Mode: "center", Listen: ln.Addr().String(), IdentityDir: t.TempDir(),
		Center: cc, TicketKeys: []ed25519.PublicKey{f.fc.TicketKey()},
		SetupLimits: hubauth.SetupLimits{Concurrent: 64, PerSecond: 64, Burst: 64, TimeoutS: 5},
	}
	if err := (*center.NodeID)(&cfg.NodeID).UnmarshalText([]byte(id)); err != nil {
		t.Fatal(err)
	}
	p, err := hubcenter.NewPolicy(cfg, host.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	f.server = hub.NewServer(hub.Options{Now: now, AfterFunc: f.clock.after, Local: host, Policy: p, Logger: f.operator.Logger})
	base := f.fc.HTTPClient().Transport
	// Negotiate a one-second heartbeat in the fixture so closed accounting and
	// the node's 45-second offline threshold also work on the accelerated clock.
	nodeHTTP := *f.fc.HTTPClient()
	nodeHTTP.Transport = centerHTTP(func(r *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(r)
		if err != nil || res.StatusCode != 200 || !strings.HasPrefix(r.URL.Path, "/v1/node/") {
			return res, err
		}
		data, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		var policy map[string]json.RawMessage
		if err := json.Unmarshal(data, &policy); err != nil {
			return nil, err
		}
		// Simulate two seconds of return transit. Node.Run immediately owes
		// its next one-second heartbeat, keeping the node online across jumps.
		if f.advanceOnHeartbeat.Load() && strings.HasSuffix(r.URL.Path, "/heartbeat") {
			f.clock.advance(2 * time.Second)
		}
		policy["heartbeat_interval_s"] = json.RawMessage("1")
		data, err = json.Marshal(policy)
		if err != nil {
			return nil, err
		}
		res.Body = io.NopCloser(bytes.NewReader(data))
		res.ContentLength = int64(len(data))
		return res, nil
	})
	api, err := center.NewClient(center.ClientOptions{Center: cc, Role: "hub", Node: cfg.NodeID, NodeKey: host.Key, Now: now, HTTP: &nodeHTTP})
	if err != nil {
		t.Fatal(err)
	}
	node := hubcenter.NewNode(hubcenter.NodeOptions{Now: now, Config: cfg, Local: host, Server: f.server, Policy: p, API: api, Logger: f.operator.Logger})
	runRuntime(t, f.ctx, func(ctx context.Context) error { return f.server.Serve(ctx, ln) })
	f.stopNode = runRuntime(t, f.ctx, node.Run)
}

func (f *receptionFixture) startOp(t *testing.T) {
	t.Helper()
	cfg, r := f.operator, f.op
	f.stopOp = runRuntime(t, f.ctx, func(ctx context.Context) error { return r.RunOperator(ctx, cfg) })
}

func (f *receptionFixture) open(t *testing.T) {
	t.Helper()
	f.startOp(t)
	f.publicSnapshots = append(f.publicSnapshots, stateWhere(t, f.ctx, f.op, func(s app.Snapshot) bool { return s.Center.Phase == "open" }))
	f.code, _ = f.op.CenterDisplay()
	if len(f.code) != 8 {
		t.Fatal("missing invitation")
	}
	f.client.CenterCode = f.code
}

func (f *receptionFixture) startCl(t *testing.T) {
	t.Helper()
	cfg, r := f.client, app.NewWithCenterHTTP(f.http)
	f.cl = r
	f.stopCl = runRuntime(t, f.ctx, func(ctx context.Context) error { return r.RunClient(ctx, cfg) })
}

func (f *receptionFixture) prompts(t *testing.T) (centerflow.SASPrompt, centerflow.SASPrompt) {
	t.Helper()
	for _, r := range []*app.Runtime{f.op, f.cl} {
		f.publicSnapshots = append(f.publicSnapshots, stateWhere(t, f.ctx, r, func(s app.Snapshot) bool { return s.Center.SASID != 0 }))
	}
	_, a := f.op.CenterDisplay()
	_, b := f.cl.CenterDisplay()
	if len(b.Code) != 6 || a.Code != b.Code {
		t.Fatal("SAS mismatch")
	}
	f.sas = b.Code
	return a, b
}

func (f *receptionFixture) accept(t *testing.T) {
	t.Helper()
	a, b := f.prompts(t)
	// The operator types what the customer reads from the client screen.
	if !f.op.VerifySAS(a.ID, b.Code[:3]+" "+b.Code[3:]) {
		t.Fatal("operator did not verify the customer's SAS")
	}
	if err := f.op.AnswerSAS(f.ctx, a.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := f.cl.AnswerSAS(f.ctx, b.ID, true); err != nil {
		t.Fatal(err)
	}
}

func (f *receptionFixture) connected(t *testing.T) app.Snapshot {
	t.Helper()
	waitPhase(t, f.ctx, f.cl, app.PhaseConnected)
	waitPhase(t, f.ctx, f.op, app.PhaseConnected)
	s := f.op.Snapshot()
	diagnosticOverSocket(t, net.TCPAddrFromAddrPort(s.Diagnostic), []byte{0x62, 0xf1, 0x90}, false, false)
	return s
}

func (f *receptionFixture) ended(t *testing.T, reason centerflow.Reason) {
	t.Helper()
	for _, r := range []*app.Runtime{f.op, f.cl} {
		s := stateWhere(t, f.ctx, r, func(s app.Snapshot) bool { return s.Center.Phase == "ended" })
		if s.Center.Reason != reason || s.Center.SASID != 0 {
			t.Fatalf("end reason %s, want %s", s.Center.Reason, reason)
		}
	}
}

func (f *receptionFixture) onlySession(t *testing.T) (string, fakecenter.SessionState) {
	t.Helper()
	m := f.fc.Snapshot()
	if len(m) != 1 {
		t.Fatalf("session count %d", len(m))
	}
	for id, s := range m {
		return id, s
	}
	panic("unreachable")
}

func TestCenterReceptionLifecycle(t *testing.T) {
	// Run after the fixture joins the two runtimes, Hub, node and TLS listeners.
	before := runtime.NumGoroutine()
	t.Cleanup(func() {
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		if got := runtime.NumGoroutine(); got > before {
			t.Errorf("goroutines after reception cleanup: %d, baseline %d", got, before)
		}
	})
	f := newReception(t, 15*time.Second)
	for _, path := range []string{"/v1/sessions", "/v1/redeem"} {
		f.fc.Inject(0, path, fakecenter.Fault{DropResponse: true})
	}
	f.open(t)
	f.startCl(t)
	f.accept(t)
	f.connected(t)
	id, remote := f.onlySession(t)
	if remote.State != "active" || f.fc.Calls(1, "/v1/sessions/"+id+"/established")+f.fc.Calls(0, "/v1/sessions/"+id+"/established") != 1 {
		t.Fatal("missing established commit")
	}
	for _, path := range []string{"/v1/sessions", "/v1/redeem"} {
		for i := range 2 {
			if f.fc.Calls(i, path) != 1 {
				t.Fatalf("entry %d %s attempts %d", i, path, f.fc.Calls(i, path))
			}
		}
	}
	for dir, peer := range map[string]*app.Runtime{f.client.StateDir: f.op, f.operator.StateDir: f.cl} {
		store, err := centerstate.Open(dir, "test-center")
		if err != nil {
			t.Fatal(err)
		}
		pins, err := store.Pins()
		if err != nil || len(pins) != 1 || center.SessionID(pins[0].Session).String() != id || pins[0].Peer != peer.Snapshot().Device {
			t.Fatal("missing reception pin", err)
		}
	}
	f.checkSecrets(t)
	f.stopCl()
	f.stopOp()
	f.operator.Center.Node = ""
	f.client.CenterCode = ""
	f.op = app.NewWithCenterHTTP(f.http)
	f.startOp(t)
	f.startCl(t)
	s := f.connected(t)
	if err := f.op.EndReception(f.ctx); err != nil {
		t.Fatal(err)
	}
	f.ended(t, centerflow.OperatorEnded)
	for _, r := range []*app.Runtime{f.op, f.cl} {
		waitPhase(t, f.ctx, r, app.PhaseStopped)
	}
	for _, dir := range []string{f.operator.StateDir, f.client.StateDir} {
		store, err := centerstate.Open(dir, "test-center")
		if err != nil {
			t.Fatal(err)
		}
		pins, err := store.Pins()
		if err != nil || len(pins) != 0 {
			t.Fatal("end retained pins", err)
		}
	}
	f.stopOp()
	f.stopCl()
	assertPortsReleased(t, net.TCPAddrFromAddrPort(s.Diagnostic), net.TCPAddrFromAddrPort(s.Control), net.UDPAddrFromAddrPort(s.Discovery))
	if len(f.server.Usage()) != 0 {
		t.Fatal("live routes after end")
	}
}

func (f *receptionFixture) checkSecrets(t *testing.T) {
	t.Helper()
	f.mu.Lock()
	secrets := append([]string{f.account, f.code, f.sas}, f.tickets...)
	f.mu.Unlock()
	if len(secrets) < 5 {
		t.Fatal("missing issued tickets")
	}
	surfaces := []string{f.logs.snapshot()}
	for _, r := range []*app.Runtime{f.op, f.cl} {
		f.publicSnapshots = append(f.publicSnapshots, <-r.Subscribe(f.ctx))
	}
	for _, s := range f.publicSnapshots {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		d, err := diagnostics.Build(diagnostics.Input{Version: "dev", Runtime: s, Permission: diagnostics.Permission{Platform: runtime.GOOS}})
		if err != nil {
			t.Fatal(err)
		}
		surfaces = append(surfaces, string(b), string(d))
	}
	for _, secret := range secrets {
		pattern := regexp.QuoteMeta(secret)
		if secret == f.sas || secret == f.code {
			pattern = "(?:^|[^[:alnum:]])" + pattern + "(?:$|[^[:alnum:]])"
		}
		match := regexp.MustCompile(pattern)
		for _, data := range surfaces {
			if secret == "" || match.MatchString(data) {
				t.Fatal("credential present in public surface or empty fixture credential")
			}
		}
	}
}

func TestCenterPairingInterrupted(t *testing.T) {
	for _, tc := range []string{"reject", "restart"} {
		t.Run(tc, func(t *testing.T) {
			f := newReception(t, 12*time.Second)
			f.open(t)
			f.startCl(t)
			a, _ := f.prompts(t)
			want := centerflow.Rejected
			if tc == "reject" {
				if err := f.op.AnswerSAS(f.ctx, a.ID, false); err != nil {
					t.Fatal(err)
				}
			} else {
				f.stopCl()
				f.client.CenterCode = ""
				f.startCl(t)
				want = centerflow.PairingLost
			}
			f.ended(t, want)
			_, s := f.onlySession(t)
			if s.Reason != string(want) {
				t.Fatal(s.Reason)
			}
		})
	}
}

func TestCenterSASTimeout(t *testing.T) {
	if os.Getenv("RENET_LONG_CENTER_TEST") != "1" {
		t.Skip("set RENET_LONG_CENTER_TEST=1 for the three-minute application SAS deadline")
	}
	f := newReception(t, 200*time.Second)
	f.open(t)
	f.startCl(t)
	f.prompts(t)
	f.ended(t, centerflow.ConfirmTimeout)
}

func TestCenterRevocation(t *testing.T) {
	f := newReception(t, 40*time.Second)
	f.open(t)
	f.startCl(t)
	f.accept(t)
	f.connected(t)
	id, _ := f.onlySession(t)
	// Keep both long polls outstanding until the Hub processes the revocation.
	// Otherwise the endpoints can tear down the route ahead of the heartbeat.
	gate := make(chan struct{})
	f.holdPolls.Store(&gate)
	f.fc.RevokeSession(id)
	for len(f.server.Usage()) != 0 {
		select {
		case <-f.ctx.Done():
			t.Fatal("Hub retained revoked route")
		case <-time.After(10 * time.Millisecond):
		}
	}
	close(gate)
	f.ended(t, centerflow.AdminRevoked)
	for {
		s := f.fc.Snapshot()[id]
		if s.Released {
			rx, tx := f.server.TrafficTotals()
			if s.Usage.RX != rx || s.Usage.TX != tx || rx == 0 || tx == 0 || len(f.server.Usage()) != 0 {
				t.Fatal("incomplete final accounting")
			}
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("missing final heartbeat")
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func TestCenterReasonDisplay(t *testing.T) {
	for _, reason := range []centerflow.Reason{centerflow.Busy, centerflow.NodeLost, centerflow.NICMissing, centerflow.CodeInvalid, centerflow.AccountExpired, centerflow.NodeUnavailable} {
		t.Run(string(reason), func(t *testing.T) {
			f := newReception(t, 10*time.Second)
			r := f.op
			switch reason {
			case centerflow.NICMissing, centerflow.CodeInvalid:
				f.open(t)
				if reason == centerflow.NICMissing {
					f.client.NICName = "absent"
				} else if f.code == "00000000" {
					f.client.CenterCode = "00000001"
				} else {
					f.client.CenterCode = "00000000"
				}
				f.startCl(t)
				r = f.cl
			case centerflow.Busy:
				// A second process resumes with the same paired device while its first tunnel is live.
				f.open(t)
				f.startCl(t)
				f.accept(t)
				f.connected(t)
				f.client.CenterCode = ""
				f.startCl(t)
				r = f.cl
			case centerflow.NodeLost, centerflow.AccountExpired:
				f.open(t)
				f.startCl(t)
				f.prompts(t)
				if reason == centerflow.NodeLost {
					f.stopNode()
					f.fc.Advance(46 * time.Second)
				} else {
					f.fc.SetAccount(f.account, "active", 1, time.Now().Add(-time.Second))
				}
				f.ended(t, reason)
				r = f.cl
			case centerflow.NodeUnavailable:
				f.fc.SetNodeState(f.operator.Center.Node, "disabled")
				f.startOp(t)
			}
			stateWhere(t, f.ctx, r, func(s app.Snapshot) bool { return s.Center.Reason == reason })
			if reason == centerflow.NICMissing && f.fc.Calls(0, "/v1/redeem")+f.fc.Calls(1, "/v1/redeem") != 0 {
				t.Fatal("redeemed without selected NIC")
			}
		})
	}
}

func TestCenterDeadlines(t *testing.T) {
	for _, reason := range []centerflow.Reason{centerflow.CodeExpired, centerflow.PairTimeout} {
		t.Run(string(reason), func(t *testing.T) {
			t.Parallel()
			f := newReception(t, 12*time.Second)
			f.open(t)
			if reason == centerflow.PairTimeout {
				id, _ := f.onlySession(t)
				path := "/v1/sessions/" + id + "/established"
				for i := range 2 {
					for range 512 {
						f.fc.Inject(i, path, fakecenter.Fault{Code: "unavailable"})
					}
				}
				f.startCl(t)
				f.accept(t)
				for f.fc.Calls(0, path)+f.fc.Calls(1, path) == 0 {
					select {
					case <-f.ctx.Done():
						t.Fatal("missing establishment attempt")
					case <-time.After(10 * time.Millisecond):
					}
				}
			}
			f.advanceOnHeartbeat.Store(true)
			stateWhere(t, f.ctx, f.op, func(s app.Snapshot) bool { return s.Center.Phase == "ended" })
			if f.op.Snapshot().Center.Reason != reason {
				t.Fatalf("end reason %s", f.op.Snapshot().Center.Reason)
			}
			if reason == centerflow.PairTimeout {
				f.ended(t, reason)
			}
			_, s := f.onlySession(t)
			if s.Reason != string(reason) || len(f.server.Usage()) != 0 || f.op.Snapshot().Generation != 0 || f.cl != nil && f.cl.Snapshot().Generation != 0 {
				t.Fatal("deadline did not end unestablished reception", s.Reason)
			}
		})
	}
}
