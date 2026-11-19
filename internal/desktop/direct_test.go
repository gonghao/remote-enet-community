package desktop

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"remote-enet/internal/app"
	"remote-enet/internal/session"
)

type directControlledRuntime struct {
	*controlledRuntime
	connect    func(context.Context, uint64) error
	disconnect func(context.Context, uint64) error
}

func (f *directControlledRuntime) ConnectMGU(ctx context.Context, gen uint64) error {
	return f.connect(ctx, gen)
}
func (f *directControlledRuntime) DisconnectMGU(ctx context.Context, gen uint64) error {
	return f.disconnect(ctx, gen)
}

func startDirectController(t *testing.T, f *directControlledRuntime) *Controller {
	t.Helper()
	c := testController(t, operatorSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	emitDirectConnected(t, c, f, 5)
	return c
}

func emitDirectConnected(t *testing.T, c *Controller, f *directControlledRuntime, generation uint64) {
	t.Helper()
	s := connected(generation)
	s.Role = session.RoleOperator
	f.emit(s)
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == generation })
}

func TestDirectStopJoinsCanceledSetup(t *testing.T) {
	f := &directControlledRuntime{controlledRuntime: newRuntime()}
	called, release := make(chan testCall, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.connect = func(ctx context.Context, generation uint64) error {
		called <- testCall{ctx, generation}
		<-ctx.Done()
		<-release
		return nil
	}
	c := startDirectController(t, f)
	done := make(chan error, 1)
	go func() { done <- c.ConnectMGU(t.Context()) }()
	call := await(t, called)
	if call.generation != 5 || !c.Snapshot().DirectWorking {
		t.Fatal("direct setup was not bound to the current generation")
	}
	if err := c.ConnectMGU(t.Context()); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate setup admitted: %v", err)
	}
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	await(t, call.ctx.Done())
	await(t, f.canceled)
	if !c.Snapshot().Stopping || !c.Snapshot().DirectWorking {
		t.Fatal("stop released the run before setup exited")
	}
	once.Do(func() { close(release) })
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("late setup success escaped cancellation: %v", err)
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().DirectWorking || c.Snapshot().Running {
		t.Fatal("stopped runtime retained direct-operation state")
	}
}

func TestDisableDoesNotFollowSetupOntoNewGeneration(t *testing.T) {
	f := &directControlledRuntime{controlledRuntime: newRuntime()}
	called, release := make(chan testCall, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	f.connect = func(ctx context.Context, generation uint64) error {
		called <- testCall{ctx, generation}
		<-ctx.Done()
		<-release
		return ctx.Err()
	}
	f.disconnect = func(context.Context, uint64) error {
		t.Error("old disable request followed a replacement generation")
		return nil
	}
	c := startDirectController(t, f)
	enabled := make(chan error, 1)
	go func() { enabled <- c.ConnectMGU(t.Context()) }()
	call := await(t, called)
	disabled := make(chan error, 1)
	go func() { disabled <- c.DisconnectMGU(t.Context()) }()
	await(t, call.ctx.Done())
	emitDirectConnected(t, c, f, 6)
	once.Do(func() { close(release) })
	if err := await(t, enabled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := await(t, disabled); !errors.Is(err, app.ErrNotConnected) {
		t.Fatalf("obsolete disable request: %v", err)
	}
	if c.Snapshot().Runtime.Generation != 6 {
		t.Fatal("old completion replaced the current generation")
	}
}

func TestDirectProductErrorsDoNotExposeInternalText(t *testing.T) {
	f := &directControlledRuntime{controlledRuntime: newRuntime()}
	f.connect = func(context.Context, uint64) error { return errors.New("private failure renet1:PRIVATE") }
	c := startDirectController(t, f)
	if err := c.ConnectMGU(t.Context()); err == nil {
		t.Fatal("failed setup was reported successful")
	}
	s := c.Snapshot()
	if strings.Contains(s.Error+strings.Join(s.Events, " "), "PRIVATE") || s.Error == "" || s.DirectWorking {
		t.Fatal("operation error leaked raw text or retained its admission")
	}
	if !s.Running || s.Runtime.Phase != app.PhaseConnected {
		t.Fatal("direct setup failure stopped the gateway runtime")
	}
}

func TestDirectCompletionCannotClaimReadyAfterWithdrawal(t *testing.T) {
	f := &directControlledRuntime{controlledRuntime: newRuntime()}
	f.connect = func(context.Context, uint64) error { return nil }
	c := startDirectController(t, f)
	if err := c.ConnectMGU(t.Context()); !errors.Is(err, app.ErrNotConnected) {
		t.Fatalf("withdrawn link was reported ready: %v", err)
	}
	if c.Snapshot().Notice == "Direct link ready." || c.Snapshot().DirectWorking {
		t.Fatal("completion contradicted the current direct state")
	}
	if c.Snapshot().Error != "Direct link canceled: vehicle connection changed." {
		t.Fatal("withdrawal did not publish its product error")
	}
}
