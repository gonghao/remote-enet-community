package centerflow

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/centerstate"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/ticket"
	"remote-enet/internal/transport"
	relay "remote-enet/internal/transport/hub"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (rt roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return rt(r) }

type intercept func(*http.Request, http.RoundTripper) (*http.Response, error)
type fixture struct {
	*testing.T
	ctx     context.Context
	fc      *fakecenter.Center
	op      *Operator
	cl      *Client
	node    center.NodeID
	nodeAPI *center.Client
	gen     uint32
	policy  *hubcenter.Policy
	path    string
	id      center.SessionID
	offset  atomic.Int64
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
func (f *fixture) check(err error) {
	f.Helper()
	if err != nil {
		f.Fatal(err)
	}
}
func until(t *testing.T, ready func() bool) {
	t.Helper()
	until := time.Now().Add(6 * time.Second)
	for !ready() {
		if time.Now().After(until) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func fixtureFor(t *testing.T, opHook, clHook intercept) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	f := &fixture{T: t, ctx: ctx}
	now := func() time.Time { return time.Now().Add(time.Duration(f.offset.Load())) }
	f.fc = fakecenter.New(t, fakecenter.Options{Entries: 2, Now: now})
	cfg := settings.Center{ID: "test-center", API: f.fc.Entries()}
	ln := must(net.Listen("tcp", "127.0.0.1:0"))
	local := must(identity.Generate())
	f.check(f.node.UnmarshalText([]byte(f.fc.RegisterNode(fakecenter.NodeConfig{FP: local.Fingerprint(), Addr: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Capacity: 5}))))
	f.nodeAPI = must(center.NewClient(center.ClientOptions{Role: "hub", AppVersion: "1.0.0", Node: f.node, NodeKey: local.Key, Center: cfg, HTTP: f.fc.HTTPClient(), Now: now}))
	f.gen = must(f.nodeAPI.Activate(ctx, center.NewIdempotencyKey(), center.Activation{Version: "1.0.0", Kids: []center.KeyID{center.KeyID(ticket.KidOf(f.fc.TicketKey()))}})).Gen
	f.policy = must(hubcenter.NewPolicy(hubcenter.Config{Version: 1, Mode: "center", IdentityDir: "test", Listen: ln.Addr().String(), Center: cfg, NodeID: f.node, TicketKeys: []ed25519.PublicKey{f.fc.TicketKey()}, SetupLimits: hubauth.SetupLimits{Concurrent: 64, PerSecond: 64, Burst: 64, TimeoutS: 10}}, local.Fingerprint()))
	f.policy.SetGeneration(f.gen)
	server := hub.NewServer(hub.Options{Policy: f.policy, Local: local})
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	deps := func(role string, hook intercept) Dependencies {
		local := must(identity.Generate())
		hc := *f.fc.HTTPClient()
		if hook != nil {
			orig := hc.Transport
			hc.Transport = roundTrip(func(r *http.Request) (*http.Response, error) { return hook(r, orig) })
		}
		api := must(center.NewClient(center.ClientOptions{Center: cfg, Role: role, AppVersion: "1.0.0", Device: local.Key, HTTP: &hc, Now: now}))
		conn, _, err := relay.New(relay.Options{Local: local, Wrap: func(ctx context.Context, c net.Conn, client bool, expect identity.Fingerprint, p transport.PathInfo) (transport.Session, error) {
			return secure.WrapStream(ctx, c, secure.Opts{Local: local, Expect: expect, Path: p, IsClient: client})
		}})
		f.check(err)
		t.Cleanup(func() { conn.Close() })
		store := must(centerstate.Open(t.TempDir(), cfg.ID))
		return Dependencies{API: api, LocalFP: local.Fingerprint(), Store: store, Connector: conn, CenterID: cfg.ID, RetryMin: 10 * time.Millisecond, RetryMax: 50 * time.Millisecond, Confirm: func(context.Context, SASPrompt) (bool, error) { return true, nil }}
	}
	f.op = NewOperator(deps("operator", opHook))
	f.cl = NewClient(ClientDeps{deps("client", clHook), func(context.Context) error { return nil }})
	_, code := f.fc.CreateAccount("test", 2, time.Time{})
	f.check(f.op.Login(ctx, code, "operator"))
	return f
}
func (f *fixture) start() {
	f.Helper()
	f.check(f.op.Start(f.ctx, f.node))
	f.id = f.op.current.remote.SessionID
	f.path = "/v1/sessions/" + f.id.String()
}
func (f *fixture) redeem() { f.Helper(); f.check(f.cl.Redeem(f.ctx, f.op.Invitation())) }

type runner interface {
	Run(context.Context, session.Options, serveFunc) error
}

func launch(ctx context.Context, flow runner, base session.Options, serve serveFunc) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- flow.Run(ctx, base, serve) }()
	return ch
}
func hold(ctx context.Context, _ *session.Session) error { <-ctx.Done(); return ctx.Err() }
func waitRun(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(7 * time.Second):
		t.Fatal("run did not return")
		return nil
	}
}
func blockRequest(r *http.Request, gate <-chan struct{}) error {
	select {
	case <-gate:
		return nil
	case <-r.Context().Done():
		return r.Context().Err()
	}
}
func TestCommitGatesAndRecovery(t *testing.T) {
	commit, observe := make(chan struct{}), make(chan struct{})
	var committing, observing atomic.Bool
	f := fixtureFor(t, func(r *http.Request, rt http.RoundTripper) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/established") {
			committing.Store(true)
			if err := blockRequest(r, commit); err != nil {
				return nil, err
			}
		}
		return rt.RoundTrip(r)
	}, func(r *http.Request, rt http.RoundTripper) (*http.Response, error) {
		resp, err := rt.RoundTrip(r)
		if err == nil && r.URL.Query().Has("since_version") {
			observing.Store(true)
			if err := blockRequest(r, observe); err != nil {
				resp.Body.Close()
				return nil, err
			}
		}
		return resp, err
	})
	f.fc.Inject(0, "/v1/sessions", fakecenter.Fault{DropResponse: true})
	f.start()
	f.fc.Inject(0, "/v1/redeem", fakecenter.Fault{DropResponse: true})
	f.redeem()
	if len(f.fc.Snapshot()) != 1 || f.fc.Calls(1, "/v1/sessions") != 1 || f.fc.Calls(1, "/v1/redeem") != 1 {
		t.Fatal("unexpected fallback")
	}
	ctx, stop := context.WithCancel(f.ctx)
	defer stop()
	available := make(chan *session.Session, 1)
	op := launch(ctx, f.op, session.Options{}, func(ctx context.Context, s *session.Session) error { available <- s; return hold(ctx, s) })
	cl := launch(ctx, f.cl, session.Options{OnTunnel: func(ctx context.Context, h proto.OpenTunnel, st transport.Stream) { _, _ = st.Write([]byte("ok")) }}, hold)
	until(t, committing.Load)
	if f.op.Snapshot().Phase == "active" || f.cl.Snapshot().Phase == "active" {
		t.Fatal("gate opened before commit")
	}
	for _, store := range []*centerstate.Store{f.op.Store, f.cl.Store} {
		_, err := store.Pin(f.id)
		f.check(err)
	}
	close(commit)
	var s *session.Session
	select {
	case s = <-available:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	until(t, observing.Load)
	header := proto.OpenTunnel{Kind: "tcp", Target: "test"}
	probe := func() string {
		st, err := s.OpenTunnel(ctx, header)
		f.check(err)
		defer st.Close()
		b, _ := io.ReadAll(st)
		return string(b)
	}
	if probe() != "" {
		t.Fatal("client accepted tunnel before active read")
	}
	close(observe)
	until(t, func() bool { return f.cl.Snapshot().Phase == "active" })
	if probe() != "ok" {
		t.Fatal("released tunnel did not reach client")
	}
	stop()
	if !errors.Is(waitRun(t, op), context.Canceled) || !errors.Is(waitRun(t, cl), context.Canceled) {
		t.Fatal("run cancellation")
	}
	// Reconstruct both flows with only the two persistent stores.
	f.op = NewOperator(f.op.Dependencies)
	f.cl = NewClient(ClientDeps{Dependencies: f.cl.Dependencies})
	f.op.Confirm = func(context.Context, SASPrompt) (bool, error) { return false, errors.New("unexpected prompt") }
	f.cl.Confirm = f.op.Confirm
	f.check(f.cl.Resume(f.ctx))
	op = launch(f.ctx, f.op, session.Options{}, hold)
	cl = launch(f.ctx, f.cl, session.Options{}, hold)
	until(t, func() bool { return f.op.Snapshot().Phase == "active" && f.cl.Snapshot().Phase == "active" })
	f.check(f.cl.End(f.ctx))
	_ = waitRun(t, cl)
	f.check(waitRun(t, op))
	for _, store := range []*centerstate.Store{f.op.Store, f.cl.Store} {
		if _, err := store.Pin(f.id); !errors.Is(err, centerstate.ErrNoPin) {
			t.Fatal(err)
		}
	}
}
func TestPrePairRestartAndNIC(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.cl.checkNIC = func(context.Context) error { return NICMissing }
	if err := f.cl.Redeem(f.ctx, f.op.Invitation()); !errors.Is(err, NICMissing) || f.fc.Calls(0, "/v1/redeem") != 0 {
		t.Fatal(err)
	}
	restarted := NewOperator(f.op.Dependencies)
	if err := restarted.Start(f.ctx, f.node); !errors.Is(err, PairingLost) || f.fc.Snapshot()[f.id.String()].Reason != "pairing_lost" {
		t.Fatal(err)
	}
}
func TestPairFailure(t *testing.T) {
	for _, why := range []string{"rejected", "confirm_timeout", "pake_failed"} {
		t.Run(why, func(t *testing.T) {
			f := fixtureFor(t, nil, nil)
			f.start()
			f.redeem()
			base := session.Options{}
			switch why {
			case "confirm_timeout":
				base.ConfirmTimeout = 40 * time.Millisecond
				f.op.Confirm = func(ctx context.Context, p SASPrompt) (bool, error) { <-ctx.Done(); return false, ctx.Err() }
			case "rejected":
				f.op.Confirm = func(context.Context, SASPrompt) (bool, error) { return false, nil }
			case "pake_failed":

				// Use a valid but different password so frame 11 actually charges both budgets.
				if f.op.Invitation() == "00000000" {
					f.cl.current.code = "11111111"
				} else {
					f.cl.current.code = "00000000"
				}
			}
			op := launch(f.ctx, f.op, base, hold)
			cl := launch(f.ctx, f.cl, session.Options{}, hold)
			if why == "pake_failed" {
				for _, left := range []int{2, 1} {
					until(t, func() bool { return f.cl.Snapshot().Phase == "retry" && f.op.Snapshot().AttemptsLeft == left })
					if f.cl.Snapshot().AttemptsLeft != left {
						t.Fatal(f.cl.Snapshot())
					}
					f.check(f.cl.Retry(f.ctx))
				}
			}
			f.check(waitRun(t, cl))
			f.check(waitRun(t, op))
			if f.fc.Snapshot()[f.id.String()].Reason != why {
				t.Fatal(f.fc.Snapshot())
			}
		})
	}
}
func TestUnauthorizedPollsWithoutMoreTickets(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	f.policy.SetGeneration(f.gen + 1)
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	op := launch(ctx, f.op, session.Options{}, hold)
	until(t, func() bool { return f.op.Snapshot().Reason == HubUnauthorized })
	polls := f.fc.Calls(0, f.path)
	until(t, func() bool { return f.fc.Calls(0, f.path) >= polls+2 })
	if f.fc.Snapshot()[f.id.String()].Tickets != 2 {
		t.Fatal("new tickets issued after second rejection")
	}
	f.fc.RevokeSession(f.id.String())
	f.check(waitRun(t, op))
	if f.op.Snapshot().Reason != "admin_revoked" {
		t.Fatal(f.op.Snapshot())
	}
}

// An interrupted accepted frame leaves only the operator's pin on disk.
type dropAccepted struct {
	TicketConnector
	once atomic.Bool
}
type dropSession struct{ transport.Session }

func (s dropSession) PairingExporter() [32]byte {
	return s.Session.(interface{ PairingExporter() [32]byte }).PairingExporter()
}
func (s dropSession) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return dropStream{st}, nil
}

type dropStream struct{ transport.Stream }

func (st dropStream) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("accepted")) {
		return 0, io.ErrClosedPipe
	}
	return st.Stream.Write(p)
}
func (d *dropAccepted) WaitTicket(ctx context.Context, ep transport.Endpoint, fp identity.Fingerprint, t []byte) (transport.Session, error) {
	s, err := d.TicketConnector.WaitTicket(ctx, ep, fp, t)
	if err == nil && d.once.CompareAndSwap(false, true) {
		return dropSession{s}, nil
	}
	return s, err
}
func TestLostDecision(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	f.op.Connector = &dropAccepted{TicketConnector: f.op.Connector}

	var prompts atomic.Int64
	f.op.Confirm = func(context.Context, SASPrompt) (bool, error) { prompts.Add(1); return true, nil }
	op := launch(f.ctx, f.op, session.Options{}, hold)
	cl := launch(f.ctx, f.cl, session.Options{}, hold)
	until(t, func() bool { return f.cl.Snapshot().Phase == "retry" })
	_, err := f.op.Store.Pin(f.id)
	f.check(err)
	if _, err := f.cl.Store.Pin(f.id); !errors.Is(err, centerstate.ErrNoPin) {
		t.Fatal(err)
	}
	f.check(f.cl.Retry(f.ctx))
	until(t, func() bool { return f.cl.Snapshot().Phase == "active" })
	if prompts.Load() != 1 {
		t.Fatal("duplicate operator prompt")
	}
	f.check(f.op.End(f.ctx))
	_ = waitRun(t, op)
	f.check(waitRun(t, cl))
}

// Keep the simulated node online while advancing the center's transaction clock.
func (f *fixture) advance(d time.Duration) {
	f.Helper()
	_, seq, _ := f.fc.NodeStatus(f.node.String())
	for d > 0 {
		step := min(d, 25*time.Second)
		f.offset.Add(int64(step))
		seq++
		_, err := f.nodeAPI.Heartbeat(f.ctx, center.Heartbeat{Gen: f.gen, Seq: seq, Version: "1.0.0", Kids: []center.KeyID{center.KeyID(ticket.KidOf(f.fc.TicketKey()))}, Sessions: []center.SessionUsage{}, Closed: []center.ClosedUsage{}})
		f.check(err)
		d -= step
	}
	f.fc.Advance(0)
}

func TestDeadline(t *testing.T) {
	for _, why := range []Reason{"code_expired", "pair_timeout"} {
		t.Run(string(why), func(t *testing.T) {
			var calls atomic.Int64
			var key atomic.Value
			var changed atomic.Bool
			hook := func(r *http.Request, rt http.RoundTripper) (*http.Response, error) {
				if strings.HasSuffix(r.URL.Path, "/established") {
					k := r.Header.Get("Idempotency-Key")
					if !key.CompareAndSwap(nil, k) && key.Load() != k {
						changed.Store(true)
					}
					calls.Add(1)
					return nil, io.ErrUnexpectedEOF
				}
				return rt.RoundTrip(r)
			}
			f := fixtureFor(t, hook, nil)
			f.start()
			var cl <-chan error
			if why == "pair_timeout" {
				f.redeem()
				cl = launch(f.ctx, f.cl, session.Options{}, hold)
			}
			op := launch(f.ctx, f.op, session.Options{}, hold)
			if cl != nil {
				until(t, func() bool { return calls.Load() >= 4 })
				if changed.Load() || f.op.Snapshot().Phase == "active" || f.cl.Snapshot().Phase == "active" {
					t.Fatal("unstable commit or premature release")
				}
			}
			f.advance(601 * time.Second)
			f.check(waitRun(t, op))
			if f.op.Snapshot().Reason != why {
				t.Fatal(f.op.Snapshot())
			}
			if cl != nil {
				f.check(waitRun(t, cl))
				if f.cl.Snapshot().Reason != why {
					t.Fatal(f.cl.Snapshot())
				}
			}
		})
	}
}

func TestMissingSessionRemovesPin(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	s := f.cl.current.remote
	f.check(f.cl.save(f.ctx, s, s.OperatorFP))
	f.check(f.op.End(f.ctx))
	f.advance(601 * time.Second)
	cl := NewClient(ClientDeps{Dependencies: f.cl.Dependencies})
	err := cl.Resume(f.ctx, f.id)
	if ReasonOf(err) != "not_found" {
		t.Fatal(err)
	}
	if _, err := cl.Store.Pin(f.id); !errors.Is(err, centerstate.ErrNoPin) {
		t.Fatal(err)
	}
}

func TestRedeemContentionAndLostReply(t *testing.T) {
	var lose atomic.Bool
	f := fixtureFor(t, nil, func(r *http.Request, rt http.RoundTripper) (*http.Response, error) {
		resp, err := rt.RoundTrip(r)
		if err == nil && r.URL.Path == "/v1/redeem" && lose.Load() {
			resp.Body.Close()
			return nil, io.ErrUnexpectedEOF
		}
		return resp, err
	})
	f.start()
	code := f.op.Invitation()
	lose.Store(true)
	if err := f.cl.Redeem(f.ctx, code); !errors.Is(err, center.ErrUnreachable) {
		t.Fatal(err)
	}
	lose.Store(false)
	f.check(f.cl.Redeem(f.ctx, code))
	if f.fc.Calls(0, "/v1/client/sessions") == 0 || f.cl.current.remote.SessionID != f.id {
		t.Fatal("missing recovery")
	}
	// A different signer cannot recover the consumed code.
	local := must(identity.Generate())
	deps := f.cl.Dependencies
	deps.LocalFP = local.Fingerprint()
	deps.API = must(center.NewClient(center.ClientOptions{Center: settings.Center{ID: "test-center", API: f.fc.Entries()}, Role: "client", Device: local.Key, AppVersion: "1.0.0", HTTP: f.fc.HTTPClient()}))
	other := NewClient(ClientDeps{deps, f.cl.checkNIC})
	if err := other.Redeem(f.ctx, code); ReasonOf(err) != CodeInvalid {
		t.Fatal(err)
	}
}

func TestAccountNodesAndLogout(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	a := must(f.op.Account(f.ctx))
	if a.Account.Label != "test" {
		t.Fatal("unexpected account")
	}
	nodes := must(f.op.Nodes(f.ctx))
	if len(nodes) != 1 || nodes[0].Latency <= 0 {
		t.Fatal(nodes)
	}
	f.start()
	f.check(f.op.Logout(f.ctx))
	if _, found, err := f.op.Store.Login(); err != nil || found {
		t.Fatal(found, err)
	}
	if _, err := f.op.Account(f.ctx); ReasonOf(err) != "unauthenticated" {
		t.Fatal(err)
	}
	if f.fc.Snapshot()[f.id.String()].Reason != "device_revoked" {
		t.Fatal("device was not revoked")
	}
}

func TestClientRestartBeforePin(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	restarted := NewClient(ClientDeps{Dependencies: f.cl.Dependencies})
	if err := restarted.Resume(f.ctx); !errors.Is(err, PairingLost) || f.fc.Snapshot()[f.id.String()].Reason != "pairing_lost" {
		t.Fatal(err)
	}
}

func TestTwoDevicesRedeemConcurrently(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	code := f.op.Invitation()
	local := must(identity.Generate())
	deps := f.cl.Dependencies
	deps.LocalFP = local.Fingerprint()
	deps.Store = must(centerstate.Open(t.TempDir(), deps.CenterID))
	deps.API = must(center.NewClient(center.ClientOptions{Center: settings.Center{ID: deps.CenterID, API: f.fc.Entries()}, Role: "client", Device: local.Key, AppVersion: "1.0.0", HTTP: f.fc.HTTPClient()}))
	second := NewClient(ClientDeps{deps, f.cl.checkNIC})
	gate := make(chan struct{})
	results := make(chan error, 2)
	for _, client := range []*Client{f.cl, second} {
		go func() { <-gate; results <- client.Redeem(f.ctx, code) }()
	}
	close(gate)
	got := map[Reason]int{}
	for range 2 {
		err := <-results
		if err == nil {
			got["success"]++
		} else {
			got[ReasonOf(err)]++
		}
	}
	if got["success"] != 1 || got[CodeInvalid] != 1 {
		t.Fatal(got)
	}
}

func TestUnknownPeerKeepsTicketAndBudget(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	ctx, cancel := context.WithCancel(f.ctx)
	defer cancel()
	cl := launch(ctx, f.cl, session.Options{}, hold)
	until(t, func() bool { return f.cl.Snapshot().Reason == OperatorWaiting })
	// Let several backoff intervals pass without registering an operator wait.
	time.Sleep(120 * time.Millisecond)
	if f.fc.Snapshot()[f.id.String()].Tickets != 1 || f.cl.Snapshot().AttemptsLeft != 3 {
		t.Fatal("idle dial spent a ticket or a pairing attempt")
	}
	cancel()
	if !errors.Is(waitRun(t, cl), context.Canceled) {
		t.Fatal("dial loop did not cancel")
	}
}

func TestForbiddenPreservesPin(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	s := f.cl.current.remote
	f.check(f.cl.save(f.ctx, s, s.OperatorFP))
	f.fc.Inject(0, f.path, fakecenter.Fault{Code: "forbidden"})
	restarted := NewClient(ClientDeps{Dependencies: f.cl.Dependencies})
	if err := restarted.Resume(f.ctx); ReasonOf(err) != Forbidden {
		t.Fatal(err)
	}
	if _, err := restarted.Store.Pin(f.id); err != nil {
		t.Fatal(err)
	}
}

func TestPeerWithoutPinEndsDataConnection(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	f.start()
	f.redeem()
	s := f.cl.current.remote
	f.check(f.cl.save(f.ctx, s, s.OperatorFP))
	restarted := NewClient(ClientDeps{Dependencies: f.cl.Dependencies})
	f.check(restarted.Resume(f.ctx))
	op := launch(f.ctx, f.op, session.Options{}, hold)
	cl := launch(f.ctx, restarted, session.Options{}, hold)
	f.check(waitRun(t, op))
	f.check(waitRun(t, cl))
	if f.fc.Snapshot()[f.id.String()].Reason != "pairing_lost" {
		t.Fatal(f.op.Snapshot(), restarted.Snapshot())
	}
}

func TestOperatorWaitsForSuccessfulCommitResponse(t *testing.T) {
	release := make(chan struct{})
	f := fixtureFor(t, func(r *http.Request, rt http.RoundTripper) (*http.Response, error) {
		resp, err := rt.RoundTrip(r)
		if err == nil && strings.HasSuffix(r.URL.Path, "/established") {
			if err := blockRequest(r, release); err != nil {
				resp.Body.Close()
				return nil, err
			}
		}
		return resp, err
	}, nil)
	f.start()
	f.redeem()
	op := launch(f.ctx, f.op, session.Options{OnTunnel: func(_ context.Context, _ proto.OpenTunnel, st transport.Stream) { _, _ = st.Write([]byte("ok")) }}, hold)
	available := make(chan *session.Session, 1)
	cl := launch(f.ctx, f.cl, session.Options{}, func(ctx context.Context, s *session.Session) error { available <- s; return hold(ctx, s) })
	var client *session.Session
	select {
	case client = <-available:
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	probe := func() string {
		st := must(client.OpenTunnel(f.ctx, proto.OpenTunnel{Kind: "reverse", Target: "test"}))
		defer st.Close()
		b, _ := io.ReadAll(st)
		return string(b)
	}
	if f.op.Snapshot().Phase != "awaiting_center" || probe() != "" {
		t.Fatal("operator served before successful response")
	}
	close(release)
	until(t, func() bool { return f.op.Snapshot().Phase == "active" })
	if probe() != "ok" {
		t.Fatal("operator did not release after successful response")
	}
	f.check(f.cl.End(f.ctx))
	_ = waitRun(t, cl)
	f.check(waitRun(t, op))
}

func TestCenterIdentityMismatch(t *testing.T) {
	f := fixtureFor(t, nil, nil)
	local := must(identity.Generate())
	deps := f.op.Dependencies
	deps.API = must(center.NewClient(center.ClientOptions{Center: settings.Center{ID: "other-center", API: f.fc.Entries()}, Role: "operator", Device: local.Key, AppVersion: "1.0.0", HTTP: f.fc.HTTPClient()}))
	// The signed client permanently excludes mismatched entries but exposes only
	// ErrUnreachable. A distinct center sentinel is needed for a more precise UI reason.
	_, err := NewOperator(deps).Account(f.ctx)
	if ReasonOf(err) != CenterUnreachable || f.fc.Calls(0, "/v1/operator/account") != 0 {
		t.Fatal(err)
	}
}
