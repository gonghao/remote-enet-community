package desktop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
)

type runCall struct {
	ctx      context.Context
	client   app.ClientConfig
	operator app.OperatorConfig
}

type testCall struct {
	ctx        context.Context
	generation uint64
}

type controlledRuntime struct {
	mu         sync.Mutex
	state      app.Snapshot
	states     chan app.Snapshot
	subContext chan context.Context
	started    chan runCall
	canceled   chan struct{}
	release    chan struct{}
	finish     chan error
	test       func(context.Context, uint64) (linktest.Result, error)
	numeric    *pairing.Manager
	issue      func(context.Context) (pairing.Display, error)
}

func newRuntime() *controlledRuntime {
	f := &controlledRuntime{state: app.Snapshot{Phase: app.PhaseIdle}, states: make(chan app.Snapshot, 32),
		subContext: make(chan context.Context, 1), started: make(chan runCall, 1), canceled: make(chan struct{}),
		release: make(chan struct{}), finish: make(chan error, 1)}
	close(f.release)
	return f
}

func (f *controlledRuntime) Snapshot() app.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return cloneRuntime(f.state)
}

func (f *controlledRuntime) Subscribe(ctx context.Context) <-chan app.Snapshot {
	f.subContext <- ctx
	f.states <- f.Snapshot()
	// Deliberately leave this channel open: the controller must cancel its
	// pump even if a runtime subscription is slow to close during teardown.
	return f.states
}

func (f *controlledRuntime) emit(s app.Snapshot) {
	f.mu.Lock()
	f.state = cloneRuntime(s)
	f.mu.Unlock()
	f.states <- cloneRuntime(s)
}

func (f *controlledRuntime) run(call runCall) error {
	f.started <- call
	var err error
	select {
	case <-call.ctx.Done():
		err = call.ctx.Err()
	case err = <-f.finish:
	}
	close(f.canceled)
	<-f.release
	s := f.Snapshot()
	s.Phase = app.PhaseStopped
	f.emit(s)
	return err
}

func (f *controlledRuntime) RunClient(ctx context.Context, cfg app.ClientConfig) error {
	return f.run(runCall{ctx: ctx, client: cfg})
}

func (f *controlledRuntime) RunOperator(ctx context.Context, cfg app.OperatorConfig) error {
	manager := pairing.NewManager(pairing.Config{Address: cfg.ShareAddress})
	f.mu.Lock()
	f.numeric = manager
	f.mu.Unlock()
	defer manager.Close()
	return f.run(runCall{ctx: ctx, operator: cfg})
}

func (f *controlledRuntime) numericManager() *pairing.Manager {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.numeric
}

func (f *controlledRuntime) IssueNumericCode(ctx context.Context) (pairing.Display, error) {
	if f.issue != nil {
		return f.issue(ctx)
	}
	return f.numericManager().Issue()
}

func (f *controlledRuntime) NumericCodeStatus() pairing.Status {
	if manager := f.numericManager(); manager != nil {
		return manager.Status()
	}
	return pairing.Status{Reason: cpace.ReasonUnavailable}
}

func (f *controlledRuntime) NumericCodeDisplay(_ context.Context, generation uint64) (pairing.Display, error) {
	return f.numericManager().Display(generation)
}

func (f *controlledRuntime) RevokeNumericCode(ctx context.Context, generation uint64) error {
	return f.numericManager().Revoke(ctx, generation)
}

func (f *controlledRuntime) TestLinkForGeneration(ctx context.Context, generation uint64, _ linktest.Options) (linktest.Result, error) {
	return f.test(ctx, generation)
}

func clientSettings() settings.Config {
	cfg := settings.Default()
	cfg.Client.Peer = (identity.Fingerprint{7}).String()
	return cfg
}

func operatorSettings() settings.Config {
	cfg := settings.Default()
	cfg.Role = settings.RoleOperator
	cfg.Operator.Endpoint, cfg.Operator.Listen, cfg.Operator.Bind = "127.0.0.1:47000", "127.0.0.1:47000", "127.0.0.1"
	cfg.Operator.Exclusive = false
	return cfg
}

func testController(t *testing.T, cfg settings.Config, modify func(*dependencies)) *Controller {
	t.Helper()
	deps := production()
	deps.load = func(string) (settings.Config, error) { return cfg, nil }
	deps.save = func(string, settings.Config) error { return nil }
	deps.interfaces = func(context.Context) ([]carside.NIC, error) { return nil, nil }
	modify(&deps)
	dir := t.TempDir()
	c, err := newController(t.Context(), Options{ConfigPath: filepath.Join(dir, "settings.json"), StateDir: filepath.Join(dir, "state")}, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("close controller: %v", err)
		}
	})
	return c
}

func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reach the synchronization point")
		var zero T
		return zero
	}
}

func stateWhere(t *testing.T, c *Controller, match func(State) bool) State {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	states := c.Subscribe(ctx)
	for {
		select {
		case s, ok := <-states:
			if !ok {
				t.Fatal("controller closed before expected state")
			}
			if match(s) {
				return s
			}
		case <-ctx.Done():
			s := c.Snapshot()
			t.Fatalf("state not reached: running=%t stopping=%t testing=%t phase=%s generation=%d notice=%q error=%q", s.Running, s.Stopping, s.Testing, s.Runtime.Phase, s.Runtime.Generation, s.Notice, s.Error)
		}
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func connected(generation uint64) app.Snapshot {
	return app.Snapshot{Role: session.RoleClient, Phase: app.PhaseConnected, Generation: generation, Peer: identity.Fingerprint{7}}
}

func TestStartRequestDoesNotOwnRuntimeAndStopMustJoin(t *testing.T) {
	f, next := newRuntime(), newRuntime()
	f.release = make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()
	var factories atomic.Int32
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime {
			if factories.Add(1) == 1 {
				return f
			}
			return next
		}
	})
	request, cancelRequest := context.WithCancel(t.Context())
	if err := c.StartClient(request, ""); err != nil {
		t.Fatal(err)
	}
	call := await(t, f.started)
	subCtx := await(t, f.subContext)
	cancelRequest()
	if call.ctx.Err() != nil || subCtx.Err() != nil {
		t.Fatal("request cancellation reached the runtime")
	}
	if call.client.Peer != (identity.Fingerprint{7}) || call.client.Invite != "" || call.client.StateDir != filepath.Join(c.opts.StateDir, "client") {
		t.Fatal("saved pairing or isolated state path not mapped to app")
	}
	f.emit(connected(3))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 3 })
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled stop wait: %v", err)
	}
	await(t, f.canceled)
	if call.ctx.Err() == nil || subCtx.Err() == nil {
		t.Fatal("stop did not cancel runtime and subscriber")
	}
	if s := c.Snapshot(); !s.Running || !s.Stopping {
		t.Fatal("run reservation was released before join")
	}
	if err := c.StartClient(t.Context(), ""); !errors.Is(err, ErrBusy) {
		t.Fatalf("restart before join: %v", err)
	}
	if err := c.Save(t.Context(), clientSettings()); !errors.Is(err, ErrBusy) {
		t.Fatalf("save before join: %v", err)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Stop(t.Context()) }()
	select {
	case <-stopDone:
		t.Fatal("stop returned before runtime joined")
	default:
	}
	release()
	if err := await(t, stopDone); err != nil {
		t.Fatal(err)
	}
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, next.started)
	if s := c.Snapshot(); s.Runtime.Generation != 0 || s.TestResult.Samples != 0 || factories.Load() != 2 {
		t.Fatal("new runtime inherited the previous run")
	}
	// A late delivery from the old subscription must not touch the new run.
	f.emit(app.Snapshot{Phase: app.PhaseBackoff, Generation: 99, LastError: "stale"})
	next.emit(connected(1))
	s := stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 1 })
	if s.Runtime.LastError != "" {
		t.Fatal("old subscription contaminated the new runtime")
	}
}

func TestConcurrentStartCloseAndParentCancellation(t *testing.T) {
	f := newRuntime()
	f.release = make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(f.release) }) }
	defer release()
	var factories atomic.Int32
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { factories.Add(1); return f }
	})
	start := make(chan struct{})
	results := make(chan error, 32)
	for range cap(results) {
		go func() { <-start; results <- c.StartClient(t.Context(), "") }()
	}
	close(start)
	accepted := 0
	for range cap(results) {
		err := await(t, results)
		if err == nil {
			accepted++
		} else if !errors.Is(err, ErrBusy) {
			t.Fatalf("concurrent start: %v", err)
		}
	}
	if accepted != 1 || factories.Load() != 1 {
		t.Fatalf("multiple runtimes admitted: accepted=%d factories=%d", accepted, factories.Load())
	}
	await(t, f.started)
	if err := c.Close(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatalf("abandoned close wait: %v", err)
	}
	await(t, f.canceled)
	if err := c.StartClient(t.Context(), ""); !errors.Is(err, ErrClosed) {
		t.Fatalf("start after close admission: %v", err)
	}
	closeResults := make(chan error, 8)
	for i := range cap(closeResults) {
		go func() {
			if i%2 == 0 {
				closeResults <- c.Close(t.Context())
			} else {
				closeResults <- c.Stop(t.Context())
			}
		}()
	}
	release()
	for range cap(closeResults) {
		if err := await(t, closeResults); err != nil {
			t.Fatal(err)
		}
	}
	for range c.Subscribe(t.Context()) {
	}
	if err := c.Save(t.Context(), clientSettings()); !errors.Is(err, ErrClosed) {
		t.Fatal("closed controller accepted persistence")
	}

	parent, cancel := context.WithCancel(t.Context())
	deps := production()
	deps.load = func(string) (settings.Config, error) { return clientSettings(), nil }
	auto := newRuntime()
	deps.newRuntime = func() appRuntime { return auto }
	child, err := newController(parent, Options{ConfigPath: "unused", StateDir: t.TempDir()}, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := child.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, auto.started)
	subscription := child.Subscribe(t.Context())
	cancel()
	await(t, child.done)
	for range subscription {
	}
	if child.Snapshot().Running {
		t.Fatal("parent cancellation did not join the runtime")
	}
}

func TestNaturalRuntimeFailureAndSaveReceipts(t *testing.T) {
	f := newRuntime()
	var saved settings.Config
	var fail bool
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { return f }
		d.save = func(_ string, cfg settings.Config) error {
			if fail {
				return errors.New("disk failed")
			}
			saved = cfg
			return nil
		}
	})
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	f.finish <- errors.New("runtime failed")
	stateWhere(t, c, func(s State) bool { return !s.Running && s.Error != "" })
	cfg := c.Snapshot().Config
	cfg.Client.NICName = "fixed-nic"
	cfg.Client.Peer = ""
	if err := c.Save(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.Notice != "Settings saved." || s.Error != "" || s.Config.Client.Peer != clientSettings().Client.Peer || !saved.Equal(s.Config) {
		t.Fatal("save did not publish a successful, peer-preserving receipt")
	}
	fail = true
	cfg.Client.NICName = "other-nic"
	if err := c.Save(t.Context(), cfg); err == nil {
		t.Fatal("failed persistence reported success")
	}
	if s := c.Snapshot(); !s.Config.Equal(saved) || s.Error == "" || s.Notice != "" {
		t.Fatal("failed save modified the baseline or kept a success notice")
	}
	fail = false
	if err := c.Save(t.Context(), saved); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Notice != "Settings saved." || c.Snapshot().Error != "" {
		t.Fatal("same-value save did not acknowledge success")
	}
}

func TestSubscriptionsOwnNestedStateAndHaveBoundedHistory(t *testing.T) {
	c := testController(t, clientSettings(), func(d *dependencies) {})
	c.mu.Lock()
	c.state.NICs = []carside.NIC{{Name: "test-nic"}}
	c.state.Runtime.Metrics = observe.Snapshot{Active: []observe.TunnelSnapshot{{Target: "active"}}, History: []observe.TunnelSnapshot{{Target: "history"}}, Events: []observe.Event{{Kind: "event"}}, LastError: &observe.Event{Error: "error"}}
	c.mu.Unlock()
	a, b := c.Subscribe(t.Context()), c.Subscribe(t.Context())
	x, y := await(t, a), await(t, b)
	x.NICs[0].Name = "modified"
	x.Runtime.Metrics.Active[0].Target = "modified"
	x.Runtime.Metrics.History[0].Target = "modified"
	x.Runtime.Metrics.Events[0].Kind = "modified"
	x.Runtime.Metrics.LastError.Error = "modified"
	if y.NICs[0].Name != "test-nic" || y.Runtime.Metrics.Active[0].Target != "active" || y.Runtime.Metrics.History[0].Target != "history" || y.Runtime.Metrics.Events[0].Kind != "event" || y.Runtime.Metrics.LastError.Error != "error" {
		t.Fatal("subscribers share nested storage")
	}
	c.mu.Lock()
	for i := range 240 {
		c.eventLocked(fmt.Sprint(i))
		c.publishLocked()
	}
	c.mu.Unlock()
	x = await(t, a)
	if len(x.Events) != 200 || x.Events[0] != "40" || x.Events[199] != "239" {
		t.Fatal("slow subscriber did not receive latest bounded history")
	}
	x.Events[0] = "modified"
	if c.Snapshot().Events[0] != "40" {
		t.Fatal("snapshot aliases event history")
	}
	if len(a) != 0 || cap(a) != 1 {
		t.Fatal("subscription is not latest-only")
	}
	if _, ok := <-c.Subscribe(canceledContext()); ok {
		t.Fatal("canceled subscription was not closed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	ended := c.Subscribe(ctx)
	cancel()
	for range ended {
	}
}

func TestRefreshPreservesMissingSelectionAndCancellation(t *testing.T) {
	cfg := clientSettings()
	cfg.Client.NICName = "missing"
	cfg.Client.NICAddr = "169.254.1.2"
	entered := make(chan struct{}, 1)
	c := testController(t, cfg, func(d *dependencies) {
		d.interfaces = func(ctx context.Context) ([]carside.NIC, error) {
			if ctx.Value("wait") != nil {
				entered <- struct{}{}
				<-ctx.Done()
				return nil, ctx.Err()
			}
			return []carside.NIC{{Name: "other"}}, nil
		}
	})
	if err := c.RefreshNICs(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); !s.Config.Equal(cfg) || len(s.NICs) != 1 {
		t.Fatal("refresh changed the saved fixed interface")
	}
	ctx, cancel := context.WithCancel(context.WithValue(t.Context(), "wait", true))
	done := make(chan error, 1)
	go func() { done <- c.RefreshNICs(ctx) }()
	await(t, entered)
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if !strings.Contains(c.Snapshot().Error, "canceled") {
		t.Fatal("canceled enumeration reported success")
	}
}

func TestCloseJoinsAtomicSaveAndStartUsesCommittedBaseline(t *testing.T) {
	for _, closing := range []bool{false, true} {
		t.Run(fmt.Sprint(closing), func(t *testing.T) {
			entered, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			f := newRuntime()
			c := testController(t, clientSettings(), func(d *dependencies) {
				d.newRuntime = func() appRuntime { return f }
				d.save = func(string, settings.Config) error { close(entered); <-release; return nil }
			})
			cfg := operatorSettings()
			cfg.Operator.Bind = "127.0.0.3"
			saved := make(chan error, 1)
			go func() { saved <- c.Save(t.Context(), cfg) }()
			await(t, entered)
			if err := c.Save(canceledContext(), cfg); !errors.Is(err, context.Canceled) || c.Snapshot().Error == "" {
				t.Fatal("canceled save did not publish a failure receipt")
			}
			startDone := make(chan error, 1)
			if closing {
				if err := c.Close(canceledContext()); !errors.Is(err, context.Canceled) {
					t.Fatal("close did not wait for the atomic save")
				}
				select {
				case <-c.done:
					t.Fatal("close finished before the writer exited")
				default:
				}
			} else {
				go func() { startDone <- c.StartOperator(t.Context()) }()
			}
			unblock()
			if err := await(t, saved); err != nil {
				t.Fatal(err)
			}
			if closing {
				if err := c.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				if s := c.Snapshot(); s.Config.Operator.Bind != cfg.Operator.Bind || s.Running {
					t.Fatal("completed atomic save was lost or started a runtime after close")
				}
			} else {
				if err := await(t, startDone); err != nil {
					t.Fatal(err)
				}
				if call := await(t, f.started); call.operator.Bind.String() != cfg.Operator.Bind {
					t.Fatal("start used the form predating the committed save")
				}
			}
		})
	}
}

func TestQueuedRequestCanBeCanceledWithoutStartingRuntime(t *testing.T) {
	var factories atomic.Int32
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { factories.Add(1); return newRuntime() }
	})
	if err := c.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- c.StartClient(request, "") }()
	cancel()
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	c.release()
	if factories.Load() != 0 || c.Snapshot().Running {
		t.Fatal("canceled queued start allocated a runtime")
	}
}

func TestStartupPreconditionsRejectBeforeRuntimeAllocation(t *testing.T) {
	for _, test := range []struct {
		name  string
		cfg   settings.Config
		start func(*Controller) error
	}{
		{"client-without-peer", settings.Default(), func(c *Controller) error { return c.StartClient(t.Context(), "") }},
		{"operator-with-client-role", clientSettings(), func(c *Controller) error { return c.StartOperator(t.Context()) }},
		{"client-with-operator-role", operatorSettings(), func(c *Controller) error { return c.StartClient(t.Context(), "") }},
		{"missing-operator-endpoint", func() settings.Config { cfg := operatorSettings(); cfg.Operator.Endpoint = ""; return cfg }(), func(c *Controller) error { return c.StartOperator(t.Context()) }},
		{"missing-presentation-IP", func() settings.Config { cfg := operatorSettings(); cfg.Operator.Bind = ""; return cfg }(), func(c *Controller) error { return c.StartOperator(t.Context()) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := testController(t, test.cfg, func(d *dependencies) {
				d.newRuntime = func() appRuntime { t.Error("invalid request allocated runtime"); return newRuntime() }
			})
			if err := test.start(c); err == nil || c.Snapshot().Running || c.Snapshot().Error == "" {
				t.Fatal("invalid startup did not publish a failure")
			}
		})
	}
}

// Done is first consulted by context.WithCancel during operation admission,
// while the controller still holds mu. Stop cannot pass that admission before
// the epoch has been captured, so the tests do not depend on scheduler timing.
type admissionContext struct {
	context.Context
	admitted chan struct{}
	once     sync.Once
}

func (ctx *admissionContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.admitted) })
	return ctx.Context.Done()
}

func TestStopInvalidatesStartAndInviteWaitingForGate(t *testing.T) {
	for _, method := range []string{"client", "operator", "invite"} {
		t.Run(method, func(t *testing.T) {
			cfg := operatorSettings()
			if method == "client" {
				cfg = clientSettings()
			}
			var factories, invitations atomic.Int32
			f := newRuntime()
			c := testController(t, cfg, func(d *dependencies) {
				d.newRuntime = func() appRuntime { factories.Add(1); return f }
				d.issueInvite = func(context.Context, app.InviteConfig) (string, error) {
					invitations.Add(1)
					return "secret", nil
				}
			})
			request := &admissionContext{Context: t.Context(), admitted: make(chan struct{})}
			invoke := func(ctx context.Context) error {
				switch method {
				case "client":
					return c.StartClient(ctx, "")
				case "operator":
					return c.StartOperator(ctx)
				default:
					_, err := c.IssueInvite(ctx)
					return err
				}
			}
			if err := c.acquire(t.Context()); err != nil {
				t.Fatal(err)
			}
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(c.release) }
			defer release()
			pending := make(chan error, 1)
			go func() { pending <- invoke(request) }()
			await(t, request.admitted)
			stopped := make(chan error, 1)
			go func() { stopped <- c.Stop(t.Context()) }()
			if err := await(t, stopped); err != nil {
				t.Fatal(err)
			}
			// Stop must return while the old request is still blocked on gate;
			// waiting for the global operations group would deadlock here.
			release()
			if err := await(t, pending); !errors.Is(err, context.Canceled) {
				t.Fatalf("pre-Stop request was not canceled: %v", err)
			}
			if s := c.Snapshot(); s.Running || s.Invitation != "" || s.Notice != "Runtime stopped." || s.Error != "" || factories.Load() != 0 || invitations.Load() != 0 {
				t.Fatal("pre-Stop request created resources or overwrote stopped state")
			}
			if err := invoke(t.Context()); err != nil {
				t.Fatalf("new epoch did not accept a fresh request: %v", err)
			}
			if method != "invite" {
				await(t, f.started)
			} else if c.Snapshot().Invitation != "secret" {
				t.Fatal("new epoch did not issue the requested invitation")
			}
			if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSuccessfulStopAndNaturalCompletionPublishFinalNotice(t *testing.T) {
	for _, natural := range []bool{false, true} {
		t.Run(fmt.Sprint(natural), func(t *testing.T) {
			f := newRuntime()
			c := testController(t, clientSettings(), func(d *dependencies) {
				d.newRuntime = func() appRuntime { return f }
			})
			if err := c.StartClient(t.Context(), ""); err != nil {
				t.Fatal(err)
			}
			await(t, f.started)
			c.mu.Lock()
			c.state.Error = "Previous operation failed."
			c.mu.Unlock()
			if natural {
				f.finish <- nil
			} else if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			s := stateWhere(t, c, func(s State) bool { return !s.Running })
			if s.Notice != "Runtime stopped." || s.Error != "" || s.Stopping || s.Runtime.Phase != app.PhaseStopped {
				t.Fatalf("stale final notice: notice=%q error=%q stopping=%t", s.Notice, s.Error, s.Stopping)
			}
		})
	}
}

func TestIdleStopDoesNotCancelOrWaitForAtomicSave(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.save = func(string, settings.Config) error { close(entered); <-release; return nil }
	})
	cfg := c.Snapshot().Config
	cfg.Client.NICName = "saved-after-stop"
	saved := make(chan error, 1)
	go func() { saved <- c.Save(t.Context(), cfg) }()
	await(t, entered)
	stopped := make(chan error, 1)
	go func() { stopped <- c.Stop(t.Context()) }()
	if err := await(t, stopped); err != nil {
		t.Fatal(err)
	}
	unblock()
	if err := await(t, saved); err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); !s.Config.Equal(cfg) || s.Notice != "Settings saved." || s.Error != "" {
		t.Fatal("stop epoch canceled or lost an admitted atomic save")
	}
}
