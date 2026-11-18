package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func TestStopJoinsCanceledTestBeforeRestart(t *testing.T) {
	f := newRuntime()
	called := make(chan testCall, 1)
	finished := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(finished) }) }
	defer release()
	f.test = func(ctx context.Context, generation uint64) (linktest.Result, error) {
		called <- testCall{ctx, generation}
		<-ctx.Done()
		<-finished
		// A late nominal success still belongs to the canceled generation.
		return linktest.Result{Samples: 99, BytesSent: 1000}, nil
	}
	c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	f.emit(connected(5))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 5 })
	testDone := make(chan error, 1)
	go func() { testDone <- c.TestLink(t.Context()) }()
	call := await(t, called)
	if call.generation != 5 || !c.Snapshot().Testing {
		t.Fatal("test did not capture the active generation")
	}
	if err := c.TestLink(t.Context()); !errors.Is(err, linktest.ErrBusy) {
		t.Fatalf("concurrent test not rejected: %v", err)
	}
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	await(t, call.ctx.Done())
	await(t, f.canceled)
	if s := c.Snapshot(); !s.Stopping || !s.Testing || !s.Running {
		t.Fatal("stop did not retain outstanding test worker")
	}
	if err := c.StartClient(t.Context(), ""); !errors.Is(err, ErrBusy) {
		t.Fatal("start accepted while the old test was alive")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop(t.Context()) }()
	release()
	if err := await(t, testDone); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled test reported success: %v", err)
	}
	if err := await(t, stopDone); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.Testing || s.Running || s.TestResult != (linktest.Result{}) {
		t.Fatal("stopped test published a late result")
	}
}

func TestLinkResultsAreIsolatedAcrossConnectionGenerations(t *testing.T) {
	f := newRuntime()
	called := make(chan testCall, 2)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	f.test = func(ctx context.Context, gen uint64) (linktest.Result, error) {
		called <- testCall{ctx, gen}
		if gen == 1 {
			<-release
			return linktest.Result{Samples: 91}, nil
		}
		return linktest.Result{Samples: 2}, nil
	}
	c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	f.emit(connected(1))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 1 })
	done := make(chan error, 1)
	go func() { done <- c.TestLink(t.Context()) }()
	old := await(t, called)
	f.emit(connected(2))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 2 })
	await(t, old.ctx.Done())
	unblock()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.TestResult.Samples != 0 || s.Testing {
		t.Fatal("old result contaminated replacement connection")
	}
	if err := c.TestLink(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.TestResult.Samples != 2 || s.Error != "" {
		t.Fatal("current-generation result was not published")
	}
	f.emit(app.Snapshot{Phase: app.PhaseDisconnected, Generation: 2})
	stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseDisconnected })
	if c.Snapshot().TestResult != (linktest.Result{}) {
		t.Fatal("disconnected session retained test measurements")
	}
}

func TestLinkFailureClassesAndRequestCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		text string
	}{
		{"unsupported", linktest.ErrUnsupported, "unsupported"},
		{"busy", linktest.ErrBusy, "busy"},
		{"timeout", context.DeadlineExceeded, "timed out"},
		{"canceled", context.Canceled, "canceled"},
		{"failed", errors.New("transport failed"), "failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRuntime()
			f.test = func(context.Context, uint64) (linktest.Result, error) { return linktest.Result{Samples: 1}, test.err }
			c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
			if err := c.TestLink(t.Context()); !errors.Is(err, app.ErrNotConnected) {
				t.Fatal("idle test did not fail")
			}
			if err := c.StartClient(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			call := await(t, f.started)
			f.emit(connected(1))
			stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected })
			if err := c.TestLink(t.Context()); !errors.Is(err, test.err) {
				t.Fatal(err)
			}
			s := c.Snapshot()
			if s.Testing || !strings.Contains(strings.ToLower(s.Error), test.text) || s.Notice != "" || s.TestResult.Samples != 1 || s.TestResult.TimedOut != errors.Is(test.err, context.DeadlineExceeded) {
				t.Fatalf("wrong failure state: error=%q testing=%t result=%+v", s.Error, s.Testing, s.TestResult)
			}
			if call.ctx.Err() != nil {
				t.Fatal("test cancellation reached runtime lifetime")
			}
		})
	}
}

func invitationCode(t *testing.T) string {
	t.Helper()
	peer := identity.Fingerprint{7}
	code, err := (invite.Invite{Version: 1, Peer: transport.PeerInfo{ID: peer, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "127.0.0.1:47000", Hub: peer}}}, Token: [16]byte{9}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	return code
}

func TestPairingPersistenceAndStaleFormMerge(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "saved", true: "failed"}[fail], func(t *testing.T) {
			cfg := settings.Default()
			f := newRuntime()
			saves := make(chan settings.Config, 4)
			c := testController(t, cfg, func(d *dependencies) {
				d.newRuntime = func() appRuntime { return f }
				d.save = func(_ string, cfg settings.Config) error {
					saves <- cfg
					if fail {
						return errors.New("write denied")
					}
					return nil
				}
			})
			code := invitationCode(t)
			if err := c.StartClient(t.Context(), code); err != nil {
				t.Fatal(err)
			}
			call := await(t, f.started)
			if call.client.Invite != code || !call.client.Peer.IsZero() {
				t.Fatal("invitation did not override saved pairing exclusively")
			}
			// A merely resolved peer must never be remembered as a paired peer.
			beforeHandshake := connected(0)
			beforeHandshake.Phase = app.PhaseConnecting
			f.emit(beforeHandshake)
			stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseConnecting })
			select {
			case <-saves:
				t.Fatal("peer persisted before handshake")
			default:
			}
			f.emit(connected(1))
			saved := await(t, saves)
			if saved.Client.Peer != (identity.Fingerprint{7}).String() {
				t.Fatal("wrong peer persisted")
			}
			if fail {
				stateWhere(t, c, func(s State) bool { return s.Error != "" })
				if s := c.Snapshot(); s.Config.Client.Peer != "" || strings.Contains(s.Notice, "saved") {
					t.Fatal("save failure claimed pairing was remembered")
				}
			} else {
				stateWhere(t, c, func(s State) bool { return s.Config.Client.Peer == saved.Client.Peer })
			}
			if c.Snapshot().Invitation != "" || strings.Contains(strings.Join(c.Snapshot().Events, " "), code) {
				t.Fatal("client invitation escaped into state or events")
			}
			if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !fail {
				// This form predates the connection and has no Peer.
				cfg.Client.NICName = "new-choice"
				if err := c.Save(t.Context(), cfg); err != nil {
					t.Fatal(err)
				}
				if s := c.Snapshot(); s.Config.Client.Peer != saved.Client.Peer || s.Config.Client.NICName != "new-choice" {
					t.Fatal("old form overwrote automatically persisted peer")
				}
			}
		})
	}
}

func TestIssueInviteClearsPreviousSecretAndJoinsOnStop(t *testing.T) {
	f := newRuntime()
	code := invitationCode(t)
	var fail bool
	entered := make(chan context.Context, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	c := testController(t, operatorSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { return f }
		d.issueInvite = func(ctx context.Context, cfg app.InviteConfig) (string, error) {
			if cfg.TTL != 15*time.Minute || cfg.Endpoint != operatorSettings().Operator.Endpoint || filepath.Base(cfg.StateDir) != "operator" {
				return "", errors.New("incorrect invite configuration")
			}
			if ctx.Value("block") != nil {
				entered <- ctx
				<-ctx.Done()
				<-release
				return code, nil
			}
			if fail {
				return "", errors.New(code)
			}
			return code, nil
		}
	})
	if got, err := c.IssueInvite(t.Context()); err != nil || got != code || c.Snapshot().Invitation != code {
		t.Fatalf("issue before listening: %v", err)
	}
	fail = true
	if got, err := c.IssueInvite(t.Context()); err == nil || got != "" || c.Snapshot().Invitation != "" {
		t.Fatal("failed replacement retained the previous invitation")
	}
	if strings.Contains(strings.Join(c.Snapshot().Events, " ")+c.Snapshot().Error, code) {
		t.Fatal("invitation leaked through an error")
	}
	if err := c.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	call := await(t, f.started)
	if call.operator.StateDir != filepath.Join(c.opts.StateDir, "operator") {
		t.Fatal("operator used the client identity directory")
	}
	fail = false
	if _, err := c.IssueInvite(t.Context()); err != nil {
		t.Fatal("cannot issue invitations during listening")
	}
	done := make(chan error, 1)
	go func() { _, err := c.IssueInvite(context.WithValue(t.Context(), "block", true)); done <- err }()
	inviteCtx := await(t, entered)
	if c.Snapshot().Invitation != "" {
		t.Fatal("pending generation kept the previous invitation")
	}
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	await(t, inviteCtx.Done())
	await(t, f.canceled)
	if !c.Snapshot().Stopping {
		t.Fatal("stop did not wait for invitation worker")
	}
	unblock()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Invitation != "" {
		t.Fatal("late invitation published after stop")
	}
}

func TestRuntimeDiagnosticsRedactSecretsAndDoNotEnterEvents(t *testing.T) {
	f := newRuntime()
	c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	code := invitationCode(t)
	s := connected(1)
	s.VIN, s.LastError = code, "failure: "+code
	s.Metrics = observe.Snapshot{Events: []observe.Event{{Message: code}}, LastError: &observe.Event{Error: code}, History: []observe.TunnelSnapshot{{Target: code}}}
	f.emit(s)
	got := stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 1 })
	if strings.Contains(got.Runtime.LastError+got.Runtime.VIN+got.Runtime.Metrics.Events[0].Message+got.Runtime.Metrics.LastError.Error+got.Runtime.Metrics.History[0].Target+strings.Join(got.Events, " "), code) {
		t.Fatal("runtime diagnostics retained invitation material")
	}
	if got.Runtime.Metrics.LastError == s.Metrics.LastError || s.Metrics.LastError.Error != code {
		t.Fatal("redaction mutated the runtime's own snapshot")
	}
	if err := c.StartClient(t.Context(), "invalid-secret"); !errors.Is(err, ErrBusy) {
		t.Fatal(err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.StartClient(t.Context(), "invalid-secret"); err == nil || strings.Contains(err.Error(), "invalid-secret") {
		t.Fatal("invalid invitation was accepted or echoed")
	}
}

func TestStopCancelsOfflineInvitationAndPublishesClear(t *testing.T) {
	entered, release := make(chan context.Context, 1), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	code := invitationCode(t)
	c := testController(t, operatorSettings(), func(d *dependencies) {
		d.issueInvite = func(ctx context.Context, _ app.InviteConfig) (string, error) {
			if ctx.Value("block") != nil {
				entered <- ctx
				<-ctx.Done()
				<-release
			}
			return code, nil
		}
	})
	if _, err := c.IssueInvite(t.Context()); err != nil {
		t.Fatal(err)
	}
	states := c.Subscribe(t.Context())
	if await(t, states).Invitation != code {
		t.Fatal("initial invitation missing from subscription")
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := await(t, states); s.Invitation != "" || s.Running || s.Notice != "Runtime stopped." || s.Error != "" {
		t.Fatal("offline stop did not publish invitation removal")
	}
	done := make(chan error, 1)
	go func() { _, err := c.IssueInvite(context.WithValue(t.Context(), "block", true)); done <- err }()
	ctx := await(t, entered)
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatal("stop did not retain pending offline invitation")
	}
	await(t, ctx.Done())
	unblock()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled offline invitation reported success")
	}
	if err := c.Stop(t.Context()); err != nil || c.Snapshot().Invitation != "" {
		t.Fatal("late secret escaped after offline stop")
	}
}

func TestCoalescedConnectionOnlyRemembersDurablyMatchingPeer(t *testing.T) {
	for _, mode := range []string{"saved", "missing", "different-endpoint", "save-failed"} {
		t.Run(mode, func(t *testing.T) {
			f := newRuntime()
			saves := make(chan settings.Config, 1)
			c := testController(t, settings.Default(), func(d *dependencies) {
				d.newRuntime = func() appRuntime { return f }
				d.save = func(_ string, cfg settings.Config) error {
					saves <- cfg
					if mode == "save-failed" {
						return errors.New("write failed")
					}
					return nil
				}
			})
			code := invitationCode(t)
			i, err := invite.Decode(code)
			if err != nil {
				t.Fatal(err)
			}
			if mode != "missing" {
				store, err := trust.Open(t.Context(), c.roleDir(settings.RoleClient))
				if err != nil {
					t.Fatal(err)
				}
				peer := i.Peer
				if mode == "different-endpoint" {
					peer.Endpoints[0].Addr = "127.0.0.1:47001"
				}
				if err := store.SavePeer(t.Context(), peer); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.StartClient(t.Context(), code); err != nil {
				t.Fatal(err)
			}
			await(t, f.started)
			// The subscription never receives connected. Only the final snapshot
			// carries a handshake generation, which also occurs on SavePeer failure.
			f.mu.Lock()
			f.state = connected(1)
			f.mu.Unlock()
			if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			s := c.Snapshot()
			if mode == "saved" {
				if s.Config.Client.Peer != i.Peer.ID.String() || s.Error != "" {
					t.Fatal("coalescing lost a successfully persisted pairing")
				}
				await(t, saves)
			} else if s.Config.Client.Peer != "" || strings.Contains(s.Notice, "saved") {
				t.Fatal("handshake generation alone was mistaken for durable pairing")
			} else if mode == "save-failed" {
				if !strings.Contains(strings.Join(s.Events, " "), "Paired peer could not be saved.") {
					t.Fatal("fallback config failure was not recorded before successful stop")
				}
				await(t, saves)
			} else {
				select {
				case <-saves:
					t.Fatal("unconfirmed pairing reached configuration persistence")
				default:
				}
			}
		})
	}
}

func TestRequestCancellationStopsOnlyLinkTest(t *testing.T) {
	f := newRuntime()
	called := make(chan testCall, 1)
	f.test = func(ctx context.Context, gen uint64) (linktest.Result, error) {
		called <- testCall{ctx, gen}
		<-ctx.Done()
		return linktest.Result{Samples: 2}, ctx.Err()
	}
	c := testController(t, clientSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	run := await(t, f.started)
	f.emit(connected(1))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected })
	request, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.TestLink(request) }()
	await(t, called)
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.Testing || !s.Running || !s.TestResult.Interrupted || !strings.Contains(s.Error, "canceled") || run.ctx.Err() != nil {
		t.Fatal("request cancellation stopped runtime or misreported partial result")
	}
}
