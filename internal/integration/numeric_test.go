package integration_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

type numericFixture struct {
	ctx      context.Context
	operator app.OperatorConfig
	client   app.ClientConfig
	logs     *cliLog
}

func newNumericFixture(t *testing.T) *numericFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Second)
	t.Cleanup(cancel)
	logs := &cliLog{}
	logger := slog.New(slog.NewTextHandler(logs, nil))
	addr := cliFreeTCPAddress(t)
	nic := carside.NIC{Name: "fixture", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}
	id := hsfz.Identification{DiagAddr: 0x10, VIN: "WBAXXXXXXX0000001", MAC: [6]byte{1, 2, 3, 4, 5, 6}}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: id.MarshalBody()}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	gateway := carside.Gateway{Address: netip.MustParseAddr("127.0.0.2"), Identification: id, IdentRaw: raw}
	return &numericFixture{ctx: ctx, logs: logs, operator: app.OperatorConfig{StateDir: t.TempDir(), Listen: addr, ShareAddress: addr, Bind: nic.Address, Logger: logger}, client: app.ClientConfig{StateDir: t.TempDir(), NICName: nic.Name, Logger: logger, Hooks: app.ClientHooks{
		Interfaces: func(context.Context) ([]carside.NIC, error) { return []carside.NIC{nic}, nil },
		Discover: func(context.Context, carside.DiscoverOptions) ([]carside.Gateway, error) {
			return []carside.Gateway{gateway}, nil
		},
	}}}
}

func waitPhase(t *testing.T, ctx context.Context, r *app.Runtime, phase app.Phase) {
	t.Helper()
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for s := range r.Subscribe(subCtx) {
		if s.Phase == phase {
			return
		}
	}
	t.Fatalf("waiting for %s: phase=%s error=%s", phase, r.Snapshot().Phase, r.Snapshot().LastError)
}

func runRuntime(t *testing.T, ctx context.Context, run func(context.Context) error) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); _ = run(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			t.Error("runtime did not stop")
		}
	}
	t.Cleanup(stop)
	return stop
}

func (f *numericFixture) listen(t *testing.T) (*app.Runtime, func()) {
	t.Helper()
	r := app.New()
	cfg := f.operator
	stop := runRuntime(t, f.ctx, func(ctx context.Context) error { return r.RunOperator(ctx, cfg) })
	waitPhase(t, f.ctx, r, app.PhaseListening)
	return r, stop
}

func (f *numericFixture) issue(t *testing.T, r *app.Runtime) pairing.Display {
	t.Helper()
	d, err := r.IssueNumericCode(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.client.DirectPair = &app.DirectPair{Address: d.Address, Code: d.Code}
	return d
}

func (f *numericFixture) fail(t *testing.T, kind app.PairingErrorKind) *app.Runtime {
	t.Helper()
	c := app.New()
	err := c.RunClient(f.ctx, f.client)
	if !errors.Is(err, kind) || c.Snapshot().Generation != 0 || c.Snapshot().LastError != string(kind) {
		t.Fatalf("kind=%v generation=%d, want %s", err, c.Snapshot().Generation, kind)
	}
	if d := f.client.DirectPair; d != nil && strings.Contains(f.logs.snapshot(), d.Code) {
		t.Fatal("pairing code in logs")
	}
	return c
}

func numericStore(t *testing.T, dir string) *trust.Store {
	t.Helper()
	s, err := trust.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (f *numericFixture) absent(t *testing.T, op identity.Fingerprint) {
	t.Helper()
	if _, err := numericStore(t, f.client.StateDir).Peer(f.ctx, op); !errors.Is(err, trust.ErrNotFound) {
		t.Fatalf("client pin: %v", err)
	}
}

func TestNumericLastAttemptAndSavedRestart(t *testing.T) {
	f := newNumericFixture(t)
	op, stop := f.listen(t)
	d := f.issue(t, op)
	wrong := "00000000"
	if d.Code == wrong {
		wrong = "00000001"
	}
	f.client.DirectPair.Code = wrong
	for left := 4; left > 0; left-- {
		c := f.fail(t, "confirmation")
		if status := op.NumericCodeStatus(); status.AttemptsLeft != left {
			t.Fatalf("remaining=%d want=%d", status.AttemptsLeft, left)
		}
		trusted, err := numericStore(t, f.operator.StateDir).Trusted(f.ctx, c.Snapshot().Device)
		if err != nil || trusted {
			t.Fatalf("unexpected trust: %v %v", trusted, err)
		}
		f.absent(t, op.Snapshot().Device)
	}
	if logs, zero := f.logs.snapshot(), (identity.Fingerprint{}).String(); !strings.Contains(logs, "operator=pending") || strings.Contains(logs, "operator="+zero) {
		t.Fatal("first pairing did not log the operator as pending")
	}
	f.client.DirectPair.Code = d.Code
	called := make(chan transport.PeerInfo, 1)
	f.client.Hooks.OnPaired = func(p transport.PeerInfo) { called <- p }
	c := app.New()
	cfg := f.client
	stopClient := runRuntime(t, f.ctx, func(ctx context.Context) error { return c.RunClient(ctx, cfg) })
	waitPhase(t, f.ctx, op, app.PhaseConnected)
	waitPhase(t, f.ctx, c, app.PhaseConnected)
	if status := op.NumericCodeStatus(); !status.Redeemed || status.Reason != cpace.ReasonUnavailable {
		t.Fatalf("successful pairing status: %+v", status)
	}
	var p transport.PeerInfo
	select {
	case p = <-called:
	default:
		t.Fatal("connected before callback")
	}
	want := transport.PeerInfo{ID: op.Snapshot().Device, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: d.Address, Hub: op.Snapshot().Device}}}
	stored, err := numericStore(t, f.client.StateDir).Peer(f.ctx, want.ID)
	if err != nil || !reflect.DeepEqual(stored, want) || !reflect.DeepEqual(p, want) {
		t.Fatal("client saved wrong peer", err)
	}
	trusted, err := numericStore(t, f.operator.StateDir).Trusted(f.ctx, c.Snapshot().Device)
	if err != nil || !trusted {
		t.Fatal("missing operator trust", err)
	}
	// Restart only the listener first: the running client has to adopt the pin.
	stop()
	op, stop = f.listen(t)
	waitPhase(t, f.ctx, op, app.PhaseConnected)
	stopClient()
	stop()
	op, stop = f.listen(t)
	f.fail(t, "unavailable") // A saved client cannot redeem the retired code.
	cfg = app.ClientConfig{StateDir: f.client.StateDir, Peer: p.ID, NICName: f.client.NICName, Hooks: f.client.Hooks, Logger: f.client.Logger}
	c = app.New()
	stopClient = runRuntime(t, f.ctx, func(ctx context.Context) error { return c.RunClient(ctx, cfg) })
	waitPhase(t, f.ctx, op, app.PhaseConnected)
	stopClient()
	stop()
	if strings.Contains(f.logs.snapshot(), d.Code) {
		t.Fatal("code appeared in logs")
	}
	// A new device at the same address fails the outer pin on both pinned paths.
	f.operator.StateDir = t.TempDir()
	replacement, _ := f.listen(t)
	rawInvite, err := (invite.Invite{Version: 1, Peer: p, Token: [16]byte{1}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{"saved", "raw invite"} {
		t.Run(source, func(t *testing.T) {
			cfg := cfg
			if source == "raw invite" {
				cfg.Peer, cfg.Invite = identity.Fingerprint{}, rawInvite
			}
			c := app.New()
			delays := make(chan time.Duration)
			retry := make(chan struct{})
			cfg.NICCheckInterval = time.Hour
			cfg.RetryMin, cfg.RetryMax = 100*time.Millisecond, time.Second
			cfg.Hooks.Wait = func(ctx context.Context, delay time.Duration) error {
				if delay != cfg.NICCheckInterval {
					select {
					case delays <- delay:
					case <-ctx.Done():
						return ctx.Err()
					}
					select {
					case <-retry:
						return nil
					case <-ctx.Done():
					}
				} else {
					<-ctx.Done()
				}
				return ctx.Err()
			}
			stop := runRuntime(t, f.ctx, func(ctx context.Context) error { return c.RunClient(ctx, cfg) })
			defer stop()
			for _, want := range []time.Duration{cfg.RetryMin, 2 * cfg.RetryMin} {
				select {
				case delay := <-delays:
					s := c.Snapshot()
					if delay != want || s.Phase != app.PhaseBackoff || s.LastError != "identity_mismatch" || s.Generation != 0 {
						t.Fatalf("unexpected retry: delay=%v phase=%s error=%s generation=%d", delay, s.Phase, s.LastError, s.Generation)
					}
				case <-f.ctx.Done():
					t.Fatal("client did not retry after identity mismatch")
				}
				if want == cfg.RetryMin {
					retry <- struct{}{}
				}
			}
			f.absent(t, replacement.Snapshot().Device)
		})
	}
}

func TestNumericRevokeAndStoreErrors(t *testing.T) {
	for _, scenario := range []string{"revoked", "remember", "save-peer"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNumericFixture(t)
			op, _ := f.listen(t)
			d := f.issue(t, op)
			if scenario == "revoked" {
				if err := op.RevokeNumericCode(f.ctx, d.Generation); err != nil {
					t.Fatal(err)
				}
				f.fail(t, "unavailable")
			} else {
				dir := f.operator.StateDir
				if scenario == "save-peer" {
					dir = f.client.StateDir
					// Corrupt the store after startup identity loading, before the first dial.
					discover := f.client.Hooks.Discover
					f.client.Hooks.Discover = func(ctx context.Context, opts carside.DiscoverOptions) ([]carside.Gateway, error) {
						if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte("{"), 0600); err != nil {
							return nil, err
						}
						return discover(ctx, opts)
					}
				} else if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
				f.fail(t, "store_failed")
				if err := os.Remove(filepath.Join(dir, "trust.json")); err != nil {
					t.Fatal(err)
				}
			}
			f.absent(t, op.Snapshot().Device)
		})
	}
}

// These scripts drive only the first control stream of a loopback test session.
func readNumericFrame[T any](t *testing.T, s transport.Stream) T {
	t.Helper()
	v, err := proto.Read(s, proto.MaxFrameLen)
	if err != nil {
		t.Fatal(err)
	}
	msg, ok := v.(T)
	if !ok {
		t.Fatalf("unexpected frame %T", v)
	}
	return msg
}

func writeNumericFrame(t *testing.T, s transport.Stream, v any) {
	t.Helper()
	if err := proto.Write(s, v); err != nil {
		t.Fatal(err)
	}
}

func openTestControl(t *testing.T, ctx context.Context, ts transport.Session, initiator bool) transport.Stream {
	t.Helper()
	var s transport.Stream
	var err error
	if initiator {
		s, err = ts.OpenStream(ctx)
	} else {
		s, err = ts.AcceptStream(ctx)
	}
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func dialNumeric(t *testing.T, ctx context.Context, addr string, id *identity.Local) transport.Session {
	t.Helper()
	c, _, err := hubtransport.New(hubtransport.Options{Local: id, Wrap: secureWrap(id)})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := c.DialDirectPair(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ts.Close(nil) })
	return ts
}

func scriptBinding(t *testing.T, ts transport.Session, client, operator identity.Fingerprint, hc, ho proto.Hello) cpace.Binding {
	t.Helper()
	hash := func(h proto.Hello) [32]byte {
		w := sha256.New()
		if err := proto.Write(w, h); err != nil {
			t.Fatal(err)
		}
		return [32]byte(w.Sum(nil))
	}
	return cpace.Binding{Exporter: ts.(interface{ PairingExporter() [32]byte }).PairingExporter(), ClientFP: client, OperatorFP: operator, HelloClientHash: hash(hc), HelloOperatorHash: hash(ho)}
}

func TestNumericStopDuringHandshake(t *testing.T) {
	f := newNumericFixture(t)
	op, stop := f.listen(t)
	d := f.issue(t, op)
	id := localIdentity(t)
	ts := dialNumeric(t, f.ctx, d.Address, id)
	s := openTestControl(t, f.ctx, ts, true)
	hc := proto.Hello{Role: "client", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}
	writeNumericFrame(t, s, hc)
	ho := readNumericFrame[proto.Hello](t, s)
	_, a, err := cpace.Start(d.Code, scriptBinding(t, ts, id.Fingerprint(), ts.Peer(), hc, ho))
	if err != nil {
		t.Fatal(err)
	}
	writeNumericFrame(t, s, proto.PakeCommit{Value: a})
	readNumericFrame[proto.PakeShareB](t, s)
	stop()
	if _, err := op.NumericCodeDisplay(f.ctx, d.Generation); !errors.Is(err, cpace.ReasonUnavailable) {
		t.Fatalf("code after stop: %v", err)
	}
	if _, err := proto.ReadPair(s); err == nil {
		t.Fatal("control remained open")
	}
	ok, err := numericStore(t, f.operator.StateDir).Trusted(f.ctx, id.Fingerprint())
	if err != nil || ok {
		t.Fatal("unexpected trust", err)
	}
}

type integrationResponder struct{ m *pairing.Manager }

func (r integrationResponder) Begin(ctx context.Context) (session.DigitalAttempt, error) {
	return r.m.Begin(ctx)
}

func TestNumericClientScripts(t *testing.T) {
	for _, scenario := range []string{"lost-confirmation", "early-accepted", "late-accepted", "untrusted-reason", "unsupported", "expired", "nic-change", "separate-tls"} {
		t.Run(scenario, func(t *testing.T) {
			f := newNumericFixture(t)
			addr := f.operator.Listen
			front := localIdentity(t)
			_, a, err := hubtransport.New(hubtransport.Options{Local: front, Wrap: secureWrap(front), Listen: addr})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close() })
			f.client.DirectPair = &app.DirectPair{Address: addr, Code: "00005123"}
			var op *app.Runtime
			var backend transport.Stream
			var fakeClient *identity.Local
			if scenario == "separate-tls" {
				f.operator.Listen = cliFreeTCPAddress(t)
				f.operator.ShareAddress = f.operator.Listen
				op, _ = f.listen(t)
				d := f.issue(t, op)
				f.client.DirectPair = &app.DirectPair{Address: addr, Code: d.Code}
				fakeClient = localIdentity(t)
				backend = openTestControl(t, f.ctx, dialNumeric(t, f.ctx, d.Address, fakeClient), true)
			}
			var changed atomic.Bool
			interfaces := f.client.Hooks.Interfaces
			f.client.Hooks.Interfaces = func(ctx context.Context) ([]carside.NIC, error) {
				if changed.Load() {
					return nil, nil
				}
				return interfaces(ctx)
			}
			f.client.NICCheckInterval = 5 * time.Millisecond
			var discoveries atomic.Int32
			discover := f.client.Hooks.Discover
			f.client.Hooks.Discover = func(ctx context.Context, o carside.DiscoverOptions) ([]carside.Gateway, error) {
				discoveries.Add(1)
				return discover(ctx, o)
			}
			client := app.New()
			done := make(chan error, 1)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			go func() { done <- client.RunClient(ctx, f.client) }()
			ts, err := a.Accept(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer ts.Close(nil)
			want := app.PairingErrorKind("interrupted")
			if scenario == "expired" {
				now := time.Now()
				m := pairing.NewManager(pairing.Config{Address: addr, Now: func() time.Time { return now }})
				defer m.Close()
				if _, err := m.Issue(); err != nil {
					t.Fatal(err)
				}
				now = now.Add(time.Hour)
				_, err = session.Run(ctx, ts, session.Options{Role: session.RoleOperator, LocalFP: front.Fingerprint(), DigitalManager: integrationResponder{m}})
				if !errors.Is(err, cpace.ReasonExpired) {
					t.Fatal(err)
				}
				want = "expired"
			} else {
				s := openTestControl(t, ctx, ts, false)
				hello := readNumericFrame[proto.Hello](t, s)
				reply := proto.Hello{Role: "operator", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}
				if backend != nil {
					writeNumericFrame(t, backend, hello)
					reply = readNumericFrame[proto.Hello](t, backend)
				}
				if scenario == "unsupported" {
					reply.Capabilities = nil
					want = "unsupported"
				}
				writeNumericFrame(t, s, reply)
				if scenario != "unsupported" {
					commit := readNumericFrame[proto.PakeCommit](t, s)
					switch scenario {
					case "separate-tls":
						writeNumericFrame(t, backend, commit)
						writeNumericFrame(t, s, readNumericFrame[proto.PakeShareB](t, backend))
						writeNumericFrame(t, backend, readNumericFrame[proto.PakeShareA](t, s))
						writeNumericFrame(t, s, readNumericFrame[proto.PakeConfirmB](t, backend))
						want = "confirmation"
					case "nic-change":
						changed.Store(true)
					case "untrusted-reason":
						writeNumericFrame(t, s, proto.PairDecision{Reason: f.client.DirectPair.Code})
					case "early-accepted":
						writeNumericFrame(t, s, proto.PairDecision{Accepted: true})
					default:
						responder, yb, err := cpace.Respond(f.client.DirectPair.Code, scriptBinding(t, ts, ts.Peer(), front.Fingerprint(), hello, reply), commit.Value)
						if err != nil {
							t.Fatal(err)
						}
						writeNumericFrame(t, s, proto.PakeShareB{Value: yb})
						ya := readNumericFrame[proto.PakeShareA](t, s)
						tb, err := responder.Open(ya.Value)
						if err != nil {
							t.Fatal(err)
						}
						writeNumericFrame(t, s, proto.PakeConfirmB{Value: tb})
						ta := readNumericFrame[proto.PakeConfirmA](t, s)
						if _, err := responder.Verify(ta.Value); err != nil {
							t.Fatal(err)
						}
						if scenario == "late-accepted" {
							writeNumericFrame(t, s, proto.PairDecision{Accepted: true})
						} else {
							ts.Close(nil)
						}
					}
				}
			}
			select {
			case err := <-done:
				if !errors.Is(err, want) {
					t.Fatalf("want %v, got %v", want, err)
				}
			case <-ctx.Done():
				t.Fatal("client retried or failed to terminate")
			}
			if discoveries.Load() != 1 || client.Snapshot().Generation != 0 {
				t.Fatal("unexpected retry or established session")
			}
			if client.Snapshot().LastError != string(want) || strings.Contains(f.logs.snapshot(), f.client.DirectPair.Code) {
				t.Fatal("unexpected diagnostic output")
			}
			f.absent(t, front.Fingerprint())
			if op != nil {
				f.absent(t, op.Snapshot().Device)
				store := numericStore(t, f.operator.StateDir)
				for _, id := range []identity.Fingerprint{fakeClient.Fingerprint(), client.Snapshot().Device} {
					trusted, err := store.Trusted(ctx, id)
					if err != nil || trusted {
						t.Fatal("unexpected trust after channel binding failure", err)
					}
				}
			}
		})
	}
}

func TestNumericInvalidInputDiagnostics(t *testing.T) {
	f := newNumericFixture(t)
	const code = "01234567"
	f.client.DirectPair = &app.DirectPair{Address: "example.invalid/" + code + ":47000", Code: code}
	c := app.New()
	err := c.RunClient(f.ctx, f.client)
	if !errors.Is(err, invite.ErrInvalidHost) || strings.Contains(c.Snapshot().LastError, code) || strings.Contains(f.logs.snapshot(), code) {
		t.Fatal("invalid input was not safely rejected")
	}
}
