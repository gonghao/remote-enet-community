package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
)

func TestMetricCallbacksStayInTheirGeneration(t *testing.T) {
	r := New()
	old := &measurements{runtime: r, direction: observe.Inbound}
	old.start()
	h := old.Begin("127.0.0.2:6801")
	h.AddIn(10)
	old.onRTT(0)
	if got := r.Snapshot().Metrics; got.Session.RTT.TotalSamples != 1 || got.Session.RTT.Latest != 0 {
		t.Fatalf("zero RTT lost: %+v", got)
	}
	old.end(errors.New("old connection ended"))
	current := &measurements{runtime: r, direction: observe.Outbound}
	current.start()
	h.AddIn(1000)
	h.End(errors.New("late end"))
	old.onRTT(time.Hour)
	old.end(errors.New("late session end"))
	current.onRTT(time.Millisecond)
	s := r.Snapshot()
	if s.Generation != 2 || s.Metrics.Session.Generation != 2 || !s.Metrics.Session.Active || s.Metrics.Session.RTT.TotalSamples != 1 || s.Metrics.Session.RTT.Latest != time.Millisecond || s.Metrics.Session.Bytes != (observe.Bytes{}) || s.Metrics.Totals.In != 10 {
		t.Fatalf("old callbacks polluted new session: %+v", s.Metrics)
	}
	neverStarted := &measurements{runtime: r, direction: observe.Outbound}
	neverStarted.end(nil)
	neverStarted.onRTT(time.Hour)
	if r.Snapshot().Generation != 2 {
		t.Fatal("late callback started another generation")
	}
	current.end(nil)
}

func TestMetricsAreIndependentAcrossSubscriptions(t *testing.T) {
	r := New()
	m := &measurements{runtime: r, direction: observe.Outbound}
	m.start()
	h := m.Begin("target")
	h.AddIn(4)
	h.End(errors.New("failed"))
	a, b := r.Subscribe(t.Context()), r.Subscribe(t.Context())
	x, y := <-a, <-b
	x.Metrics.History[0].Target = "changed"
	x.Metrics.Events[0].Kind = "changed"
	x.Metrics.LastError.Error = "changed"
	if y.Metrics.History[0].Target == "changed" || y.Metrics.Events[0].Kind == "changed" || y.Metrics.LastError.Error == "changed" {
		t.Fatal("subscribers share metric storage")
	}
	r.update(func(*Snapshot) {})
	x, y = <-a, <-b
	x.Metrics.History[0].Target = "changed-again"
	if y.Metrics.History[0].Target == "changed-again" || r.Snapshot().Metrics.History[0].Target == "changed-again" {
		t.Fatal("published metrics alias subscriber or tracker")
	}
	m.end(nil)
	r.finish(nil)
}

func TestTestLinkWithoutSessionReturnsFailure(t *testing.T) {
	r := New()
	result, err := r.TestLink(t.Context(), linktest.Options{})
	if !errors.Is(err, ErrNotConnected) || !result.Interrupted || result.Samples != 0 {
		t.Fatalf("no session presented as success: %+v %v", result, err)
	}
}

func TestExclusiveWithoutPlatformSupportStopsBeforeStartup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has a native resolver")
	}
	r := New()
	err := r.RunOperator(t.Context(), OperatorConfig{StateDir: t.TempDir(), Listen: freeTCPAddress(t), Bind: netip.MustParseAddr("127.0.0.1"), Exclusive: true})
	if !errors.Is(err, exclusive.ErrUnsupported) || r.Snapshot().Phase != PhaseStopped {
		t.Fatalf("unsupported exclusive mode entered accept loop: %v", err)
	}
}

type ownerResolver struct{ owner exclusive.Identity }

func (r ownerResolver) Resolve(ctx context.Context, _, _ netip.AddrPort) (exclusive.Identity, error) {
	if err := ctx.Err(); err != nil {
		return exclusive.Identity{}, err
	}
	return r.owner, nil
}

func TestForwardingFailureIsObserved(t *testing.T) {
	nic, gateway := gatewayState(t)
	r := New()
	m := &measurements{runtime: r, direction: observe.Inbound}
	f, err := carside.NewForwarder(nic, quietLogger(), carside.WithObserver(m))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(gateway.Address, 0)))
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	remote, err := a.OpenStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	local, err := b.AcceptStream(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	f.HandleTunnel(t.Context(), proto.OpenTunnel{Kind: "tcp", Target: addr}, local)
	s := r.Snapshot().Metrics
	if s.ActiveConnections != 0 || len(s.History) != 1 || s.History[0].Error == "" || s.LastError == nil {
		t.Fatalf("dial failure missing from metrics: %+v", s)
	}
	m.end(nil)
}

func TestObservationCapacityDoesNotLimitForwarding(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	nic, gateway := gatewayState(t)
	r := New()
	r.metrics = observe.New(observe.Options{ActiveTunnels: 1})
	m := &measurements{runtime: r, direction: observe.Inbound}
	occupied := m.Begin("occupied")
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(gateway.Address, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	backendDone := make(chan error, 1)
	go func() {
		c, err := ln.AcceptTCP()
		if err != nil {
			backendDone <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(2 * time.Second))
		data, err := io.ReadAll(c)
		if err == nil {
			_, err = c.Write(data)
		}
		if err == nil {
			err = c.CloseWrite()
		}
		backendDone <- err
	}()
	f, err := carside.NewForwarder(nic, quietLogger(), carside.WithObserver(m))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	remote, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	local, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.HandleTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: ln.Addr().String()}, local)
	}()
	if _, err = remote.Write([]byte("opaque")); err != nil {
		t.Fatal(err)
	}
	remote.CloseWrite()
	data, err := io.ReadAll(remote)
	if err != nil || string(data) != "opaque" {
		t.Fatalf("metrics capacity disrupted transparent half-close: %q %v", data, err)
	}
	ended(t, done)
	if err := awaitError(t, backendDone); err != nil {
		t.Fatal(err)
	}
	s := r.Snapshot().Metrics
	if !r.Snapshot().MetricsIncomplete || s.LastError == nil || s.LastError.Kind != "metrics-incomplete" || s.LastError.Error != observe.ErrLimit.Error() {
		t.Fatalf("partial measurements not marked: %+v", s)
	}
	occupied.End(nil)
	m.end(nil)
}
