package app

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/packetio"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

type directTestDevice struct {
	packetio.Device
	closed   atomic.Int32
	closeErr error
	mtu      int
}

func (d *directTestDevice) MTU() int {
	if d.mtu != 0 {
		return d.mtu
	}
	return d.Device.MTU()
}
func (d *directTestDevice) Name() string        { return "memory-direct" }
func (d *directTestDevice) Local() netip.Prefix { return netip.MustParsePrefix("192.0.2.1/32") }
func (d *directTestDevice) Close() error {
	d.closed.Add(1)
	return errors.Join(d.Device.Close(), d.closeErr)
}

func memoryDirectDevice(t *testing.T) *directTestDevice {
	t.Helper()
	a, b, err := packetio.NewPair(1500, 8)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return &directTestDevice{Device: a}
}

type directFixture struct {
	r      *Runtime
	client *session.Session
	cancel context.CancelFunc
	done   chan connectionResult
	states <-chan Snapshot
}

func newDirectFixture(t *testing.T, cfg OperatorConfig, handle func(context.Context, proto.OpenTunnel, transport.Stream)) *directFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	store, _ := storeForTest(t)
	token := tokenForTest(t, store)
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	r := New()
	states := r.Subscribe(ctx)
	cfg.Bind = netip.MustParseAddr("127.0.0.1")
	done := make(chan connectionResult, 1)
	go func() {
		fatal, err := r.operatorConnection(ctx, b, cfg, store, quietLogger())
		done <- connectionResult{fatal: fatal, err: err}
	}()
	client, err := session.Run(ctx, a, session.Options{Role: session.RoleClient, Initiator: true, Token: token, OnTunnel: handle})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	nic, gateway := gatewayState(t)
	if err = client.Send(gateway.VehicleState(nic, 1)); err != nil {
		cancel()
		t.Fatal(err)
	}
	f := &directFixture{r: r, client: client, cancel: cancel, done: done, states: states}
	t.Cleanup(func() { cancel(); result(t, done) })
	f.wait(t, func(s Snapshot) bool {
		return s.Phase == PhaseConnected && (cfg.AutoMGU || s.Direct.Phase == DirectDisabled)
	})
	return f
}
func (f *directFixture) wait(t *testing.T, match func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case s := <-f.states:
			if match(s) {
				return s
			}
		case <-timer.C:
			t.Fatalf("state timeout: %+v", f.r.Snapshot())
			return Snapshot{}
		}
	}
}
func mguReply(ctx context.Context, _ proto.OpenTunnel, stream transport.Stream) {
	defer stream.Close()
	if _, err := hsfz.ReadFrame(stream, 1024); err != nil {
		return
	}
	_ = hsfz.WriteFrame(stream, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0x63, 0xf4, 0x62, 0x17, 0x2a, 0, 10, 99, 0, 8, 255, 255, 255, 0, 10, 99, 0, 1}})
}

func TestDirectRequestLifetimeAndGeneration(t *testing.T) {
	device := memoryDirectDevice(t)
	var resourceCtx context.Context
	var calls atomic.Int32
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(ctx context.Context, targets []netip.Addr) (DirectDevice, error) {
		calls.Add(1)
		resourceCtx = ctx
		if len(targets) != 1 || targets[0] != netip.MustParseAddr("10.99.0.8") {
			return nil, errors.New("wrong targets")
		}
		return device, nil
	}}, mguReply)
	gen := f.r.Snapshot().Generation
	if err := f.r.ConnectMGU(t.Context(), gen+1); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	if err := f.r.ConnectMGU(ctx, gen); err != nil {
		t.Fatal(err)
	}
	cancel()
	if resourceCtx.Err() != nil || device.closed.Load() != 0 {
		t.Fatal("caller cancellation destroyed ready link")
	}
	state := f.r.Snapshot()
	if state.Direct.Phase != DirectReady || state.Direct.Local != device.Local().Addr() || state.Direct.Address != netip.MustParseAddr("10.99.0.8") {
		t.Fatal(state.Direct)
	}
	if err := f.r.ConnectMGU(t.Context(), gen); err != nil || calls.Load() != 1 {
		t.Fatalf("duplicate connect: %v calls=%d", err, calls.Load())
	}
	if err := f.r.DisconnectMGU(t.Context(), gen+1); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
	if device.closed.Load() != 0 {
		t.Fatal("stale disconnect closed link")
	}
	if err := f.r.DisconnectMGU(t.Context(), gen); err != nil {
		t.Fatal(err)
	}
	if resourceCtx.Err() == nil || device.closed.Load() != 1 || f.r.Snapshot().Direct.Phase != DirectDisabled {
		t.Fatal("link not reclaimed")
	}
}

func TestDirectFailurePreservesGateway(t *testing.T) {
	var requests atomic.Int32
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) {
		t.Error("factory after NRC")
		return nil, errors.New("unexpected")
	}}, func(ctx context.Context, h proto.OpenTunnel, s transport.Stream) {
		defer s.Close()
		requests.Add(1)
		if _, err := hsfz.ReadFrame(s, 1024); err != nil {
			return
		}
		_ = hsfz.WriteFrame(s, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0x63, 0xf4, 0x7f, 0x22, 0x31}})
	})
	before := f.r.Snapshot()
	if requests.Load() != 0 {
		t.Fatal("default session sent MGU request")
	}
	if err := f.r.ConnectMGU(t.Context(), before.Generation); err == nil {
		t.Fatal("NRC succeeded")
	}
	after := f.r.Snapshot()
	if after.Phase != PhaseConnected || after.Gateway != before.Gateway || after.Direct.Phase != DirectFailed || requests.Load() != 1 {
		t.Fatalf("after NRC: %+v requests=%d", after, requests.Load())
	}
	conn, err := net.Dial("tcp4", after.Diagnostic.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	_, _ = conn.Write([]byte{0, 0, 0, 0, 0, 1})
	var b [1]byte
	_, _ = conn.Read(b[:])
	if requests.Load() != 2 {
		t.Fatal("HSFZ unavailable after MGU failure")
	}
}

func TestDirectDisableCancelsAndJoinsFactory(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(ctx context.Context, _ []netip.Addr) (DirectDevice, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}}, mguReply)
	enabled := make(chan error, 1)
	go func() { enabled <- f.r.ConnectMGU(t.Context(), 0) }()
	<-entered
	disabled := make(chan error, 1)
	go func() { disabled <- f.r.DisconnectMGU(t.Context(), 0) }()
	select {
	case err := <-disabled:
		t.Fatalf("disable did not join: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-enabled; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-disabled; err != nil {
		t.Fatal(err)
	}
	if f.r.Snapshot().Direct.Phase != DirectDisabled {
		t.Fatal(f.r.Snapshot().Direct)
	}
}

func TestDirectGatewayChangeDoesNotReplay(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) {
		return nil, errors.New("unexpected factory")
	}}, func(ctx context.Context, _ proto.OpenTunnel, s transport.Stream) {
		defer s.Close()
		requests.Add(1)
		once.Do(func() { close(entered) })
		var b [1]byte
		_, _ = s.Read(b[:])
		<-ctx.Done()
	})
	finished := make(chan error, 1)
	go func() { finished <- f.r.ConnectMGU(t.Context(), 0) }()
	<-entered
	nic, gateway := gatewayState(t)
	gateway.Address = netip.MustParseAddr("127.0.0.3")
	if err := f.client.Send(gateway.VehicleState(nic, 2)); err != nil {
		t.Fatal(err)
	}
	f.wait(t, func(s Snapshot) bool { return s.Gateway == gateway.Address && s.Direct.Phase == DirectDisabled })
	if err := <-finished; err == nil {
		t.Fatal("old query survived gateway change")
	}
	if requests.Load() != 1 {
		t.Fatal("query replayed")
	}
}

func TestDirectSetupFailureAndCleanupError(t *testing.T) {
	failure := errors.New("native permission denied")
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { return nil, failure }}, mguReply)
	if err := f.r.ConnectMGU(t.Context(), 0); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if state := f.r.Snapshot(); state.Phase != PhaseConnected || state.Direct.Phase != DirectFailed {
		t.Fatal(state)
	}
	device := memoryDirectDevice(t)
	device.closeErr = errors.New("route cleanup failed")
	f.r.mu.Lock()
	m := f.r.direct
	f.r.mu.Unlock()
	m.factory = func(context.Context, []netip.Addr) (DirectDevice, error) { return device, nil }
	if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.r.DisconnectMGU(t.Context(), 0); !errors.Is(err, device.closeErr) {
		t.Fatal(err)
	}
	if state := f.r.Snapshot().Direct; state.Phase != DirectFailed || state.LastError == "" {
		t.Fatal(state)
	}
	if err := f.r.DisconnectMGU(t.Context(), 0); !errors.Is(err, device.closeErr) {
		t.Fatal("cleanup error lost", err)
	}
}

func TestDirectStopCancelsFactoryAndJoins(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(ctx context.Context, _ []netip.Addr) (DirectDevice, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return nil, ctx.Err()
	}}, mguReply)
	finished := make(chan error, 1)
	go func() { finished <- f.r.ConnectMGU(t.Context(), 0) }()
	<-entered
	f.cancel()
	select {
	case <-f.done:
		t.Fatal("session returned before setup joined")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// The fixture cleanup receives the connection result and verifies termination.
}

func TestDirectOldManagerCannotPublish(t *testing.T) {
	r := New()
	old := &directManager{runtime: r}
	current := &directManager{runtime: r}
	r.direct = current
	current.publish(DirectState{Phase: DirectReady, Address: netip.MustParseAddr("10.99.0.8")})
	old.publish(DirectState{Phase: DirectFailed, LastError: "obsolete"})
	if state := r.Snapshot().Direct; state.Phase != DirectReady || state.LastError != "" {
		t.Fatal(state)
	}
}

func TestDirectAutoMGUExplicitOptIn(t *testing.T) {
	device := memoryDirectDevice(t)
	f := newDirectFixture(t, OperatorConfig{AutoMGU: true, DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { return device, nil }}, mguReply)
	if f.r.Snapshot().Direct.Phase != DirectReady {
		f.wait(t, func(s Snapshot) bool { return s.Direct.Phase == DirectReady })
	}
	if err := f.r.DisconnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if device.closed.Load() != 1 {
		t.Fatal("auto link not reclaimed")
	}
}

func TestDirectL3FailureRollsBackDevice(t *testing.T) {
	device := memoryDirectDevice(t)
	device.mtu = 100
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { return device, nil }}, mguReply)
	if err := f.r.ConnectMGU(t.Context(), 0); err == nil {
		t.Fatal("invalid device accepted")
	}
	if device.closed.Load() != 1 || f.r.Snapshot().Direct.Phase != DirectFailed || f.r.Snapshot().Phase != PhaseConnected {
		t.Fatal("failed L3 did not roll back independently")
	}
}

func TestDirectNICChangeRevokesReadyLink(t *testing.T) {
	device := memoryDirectDevice(t)
	var calls atomic.Int32
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { calls.Add(1); return device, nil }}, mguReply)
	if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	nic, gateway := gatewayState(t)
	nic.Address = netip.MustParseAddr("127.0.0.4")
	if err := f.client.Send(gateway.VehicleState(nic, 2)); err != nil {
		t.Fatal(err)
	}
	f.wait(t, func(s Snapshot) bool { return s.NIC.Address == nic.Address && s.Direct.Phase == DirectDisabled })
	if device.closed.Load() != 1 || calls.Load() != 1 {
		t.Fatal("NIC update retained or replayed old direct link")
	}
}

func TestNativeDirectFactoryFailureReturnsNilInterface(t *testing.T) {
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	for _, test := range []struct {
		name    string
		ctx     context.Context
		targets []netip.Addr
	}{
		{name: "canceled", ctx: canceled, targets: []netip.Addr{netip.MustParseAddr("10.99.0.8")}},
		{name: "invalid target", ctx: t.Context(), targets: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			device, err := nativeDirectFactory(test.ctx, test.targets)
			if device != nil || err == nil {
				t.Fatalf("factory failure: device=%#v err=%v", device, err)
			}
			if test.name == "canceled" && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}

func assertDirectHSFZ(t *testing.T, f *directFixture) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", f.r.Snapshot().Diagnostic.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if err := hsfz.WriteFrame(conn, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22, 0x17, 0x2a}}); err != nil {
		t.Fatal(err)
	}
	if _, err := hsfz.ReadFrame(conn, 1024); err != nil {
		t.Fatalf("HSFZ unavailable: %v", err)
	}
}

func TestDirectSlowFactoryAllowsHSFZAndJoinsCancellation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	device := memoryDirectDevice(t)
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(ctx context.Context, _ []netip.Addr) (DirectDevice, error) {
		close(entered)
		<-ctx.Done()
		<-release
		return device, ctx.Err()
	}}, mguReply)
	ctx, cancel := context.WithCancel(t.Context())
	finished := make(chan error, 1)
	go func() { finished <- f.r.ConnectMGU(ctx, 0) }()
	<-entered
	assertDirectHSFZ(t, f)
	cancel()
	select {
	case <-finished:
		t.Fatal("setup returned before factory joined")
	default:
	}
	close(release)
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if device.closed.Load() != 1 {
		t.Fatal("partial device leaked")
	}
	assertDirectHSFZ(t, f)
}

func TestDirectDeviceFailureHealthAndRetry(t *testing.T) {
	for _, cleanupFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "cleanup failure"}[cleanupFailure], func(t *testing.T) {
			device := memoryDirectDevice(t)
			if cleanupFailure {
				device.closeErr = errors.New("cleanup failed")
			}
			var calls atomic.Int32
			f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) {
				if calls.Add(1) > 1 {
					return nil, errors.New("replacement unavailable")
				}
				return device, nil
			}}, mguReply)
			if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
				t.Fatal(err)
			}
			f.r.mu.Lock()
			m := f.r.direct
			f.r.mu.Unlock()
			m.mu.Lock()
			oldStack := m.stack
			m.mu.Unlock()
			_ = device.Device.Close()
			select {
			case <-oldStack.Done():
			case <-time.After(time.Second):
				t.Fatal("L3 did not detect device close")
			}
			if err := f.r.ConnectMGU(t.Context(), 0); err == nil {
				t.Fatal("dead link accepted")
			}
			if s := f.r.Snapshot(); s.Direct.Phase != DirectFailed || s.Phase != PhaseConnected {
				t.Fatal(s)
			}
			if device.closed.Load() != 1 {
				t.Fatal("failed device not reclaimed")
			}
			assertDirectHSFZ(t, f)
			if err := f.r.ConnectMGU(t.Context(), 0); err == nil {
				t.Fatal("retry falsely succeeded")
			}
			if cleanupFailure {
				if calls.Load() != 1 {
					t.Fatal("unsafe restart after cleanup failure")
				}
				if err := f.r.DisconnectMGU(t.Context(), 0); !errors.Is(err, device.closeErr) {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestDirectTickDetectsDeviceFailure(t *testing.T) {
	device := memoryDirectDevice(t)
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) { return device, nil }}, mguReply)
	if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	_ = device.Device.Close()
	f.wait(t, func(s Snapshot) bool { return s.Direct.Phase == DirectFailed })
	if device.closed.Load() != 1 {
		t.Fatal("tick did not reclaim device")
	}
	assertDirectHSFZ(t, f)
}

func TestDirectOldStackCannotInvalidateReplacement(t *testing.T) {
	first, second := memoryDirectDevice(t), memoryDirectDevice(t)
	var calls atomic.Int32
	f := newDirectFixture(t, OperatorConfig{DirectFactory: func(context.Context, []netip.Addr) (DirectDevice, error) {
		if calls.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	}}, mguReply)
	if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	f.r.mu.Lock()
	m := f.r.direct
	f.r.mu.Unlock()
	m.mu.Lock()
	old := m.stack
	m.mu.Unlock()
	if err := f.r.DisconnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	if err := m.checkHealth(); err != nil {
		t.Fatal("normal disable marked failed", err)
	}
	if err := f.r.ConnectMGU(t.Context(), 0); err != nil {
		t.Fatal(err)
	}
	<-old.Done()
	if err := m.checkHealth(); err != nil {
		t.Fatal(err)
	}
	if state := f.r.Snapshot().Direct; state.Phase != DirectReady || second.closed.Load() != 0 {
		t.Fatal(state)
	}
	// The fixture's cancellation joins the replacement stack and its device.
}
