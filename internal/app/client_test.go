package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
)

type waitRequest struct {
	ctx     context.Context
	delay   time.Duration
	release chan struct{}
	done    chan struct{}
}

type scriptedNetwork struct {
	mu          sync.Mutex
	nics        []carside.NIC
	gateways    []carside.Gateway
	enumErr     error
	discoveries []carside.NIC
	waits       chan waitRequest
}

func networkForTest(nic carside.NIC, gateway carside.Gateway) *scriptedNetwork {
	return &scriptedNetwork{nics: []carside.NIC{nic}, gateways: []carside.Gateway{gateway}, waits: make(chan waitRequest, 64)}
}

func (n *scriptedNetwork) interfaces(ctx context.Context) ([]carside.NIC, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]carside.NIC(nil), n.nics...), n.enumErr
}

func (n *scriptedNetwork) discover(ctx context.Context, o carside.DiscoverOptions) ([]carside.Gateway, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.discoveries = append(n.discoveries, o.NIC)
	return append([]carside.Gateway(nil), n.gateways...), nil
}

func (n *scriptedNetwork) wait(ctx context.Context, d time.Duration) error {
	request := waitRequest{ctx: ctx, delay: d, release: make(chan struct{}), done: make(chan struct{})}
	defer close(request.done)
	select {
	case n.waits <- request:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-request.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// manualNICWait keeps NIC changes deterministic without freezing real network
// retry timers when the Hub is briefly busy during remote teardown.
func (n *scriptedNetwork) manualNICWait(interval time.Duration) func(context.Context, time.Duration) error {
	return func(ctx context.Context, delay time.Duration) error {
		if delay == interval {
			return n.wait(ctx, delay)
		}
		return pause(ctx, delay)
	}
}

func (n *scriptedNetwork) nextWait(t *testing.T, delay time.Duration) waitRequest {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case w := <-n.waits:
			if w.ctx.Err() != nil {
				continue
			}
			if w.delay != delay {
				t.Fatalf("wait duration = %s, want %s", w.delay, delay)
			}
			return w
		case <-timer.C:
			t.Fatal("wait request not received")
			return waitRequest{}
		}
	}
}

func (n *scriptedNetwork) setNICs(nics ...carside.NIC) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.nics = append([]carside.NIC(nil), nics...)
}

func (n *scriptedNetwork) discoveryHistory() []carside.NIC {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]carside.NIC(nil), n.discoveries...)
}

func snapshotWhere(t *testing.T, ch <-chan Snapshot, match func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case s, ok := <-ch:
			if !ok {
				t.Fatal("runtime stopped before expected snapshot")
				return Snapshot{}
			}
			if match(s) {
				return s
			}
		case <-timer.C:
			t.Fatal("expected snapshot not received")
			return Snapshot{}
		}
	}
}

func connectedGeneration(t *testing.T, ch <-chan Snapshot, gen uint64) Snapshot {
	t.Helper()
	return snapshotWhere(t, ch, func(s Snapshot) bool { return s.Phase == PhaseConnected && s.Generation == gen })
}

type dialFunc func(context.Context, transport.PeerInfo) (transport.Session, error)

func (f dialFunc) Dial(ctx context.Context, peer transport.PeerInfo) (transport.Session, error) {
	return f(ctx, peer)
}

type testPeer struct {
	transport transport.Session
	session   *session.Session
}

type clientLoopOptions struct {
	wait      func(context.Context, time.Duration) error
	connector func(transport.Connector) transport.Connector
}

func startClientLoop(t *testing.T, n *scriptedNetwork, fixed bool, options ...clientLoopOptions) (*Runtime, ClientConfig, <-chan Snapshot, <-chan testPeer, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	store, dir := storeForTest(t)
	peer := transport.PeerInfo{ID: identity.Fingerprint{2}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{2}}}}
	cfg := ClientConfig{StateDir: dir, Peer: peer.ID, NICName: "test", Logger: quietLogger(), NICCheckInterval: time.Hour, DiscoveryRetryInterval: 2 * time.Hour,
		Hooks: ClientHooks{Interfaces: n.interfaces, Discover: n.discover, Wait: n.wait}}
	if fixed {
		cfg.NICAddr = netip.MustParseAddr("127.0.0.1")
	}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	servers := make(chan testPeer, 16)
	var workers sync.WaitGroup
	var connector transport.Connector = dialFunc(func(ctx context.Context, peer transport.PeerInfo) (transport.Session, error) {
		a, b := memtransport.Pair(t, identity.Fingerprint{1}, peer.ID)
		workers.Add(1)
		go func() {
			defer workers.Done()
			s, err := session.Run(ctx, b, session.Options{Role: session.RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }, Logger: quietLogger()})
			if err == nil {
				select {
				case servers <- testPeer{b, s}:
				case <-ctx.Done():
				}
			}
		}()
		return a, nil
	})
	for _, option := range options {
		if option.wait != nil {
			cfg.Hooks.Wait = option.wait
		}
		if option.connector != nil {
			connector = option.connector(connector)
		}
	}
	r := New()
	states := r.Subscribe(ctx)
	if err := r.begin(session.RoleClient); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		defer close(done)
		err := r.clientLoop(ctx, cfg, connector, peer, [16]byte{}, store)
		r.finish(err)
		workers.Wait()
		result <- err
	}()
	t.Cleanup(func() { cancel(); ended(t, done) })
	return r, cfg, states, servers, cancel, result
}

func takePeer(t *testing.T, peers <-chan testPeer) testPeer {
	t.Helper()
	select {
	case peer := <-peers:
		return peer
	case <-time.After(3 * time.Second):
		t.Fatal("peer handshake did not finish")
		return testPeer{}
	}
}

func ended(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("old worker or session survived")
	}
}

func TestClientWaitsForInterfaceAndVehicle(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	n.nics, n.gateways = nil, nil
	r, cfg, states, peers, cancel, result := startClientLoop(t, n, false)
	snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseWaitingNIC })
	waitNIC := n.nextWait(t, cfg.NICCheckInterval)
	n.setNICs(nic)
	close(waitNIC.release)
	snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseWaitingVehicle })
	waitVehicle := n.nextWait(t, cfg.DiscoveryRetryInterval)
	n.mu.Lock()
	n.gateways = []carside.Gateway{gateway}
	n.mu.Unlock()
	close(waitVehicle.release)
	got := connectedGeneration(t, states, 1)
	_ = takePeer(t, peers)
	if got.NIC != nic || got.Gateway != gateway.Address || got.VIN != gateway.Identification.VIN {
		t.Fatalf("wrong selection: %+v", got)
	}
	monitor := n.nextWait(t, cfg.NICCheckInterval)
	n.mu.Lock()
	n.gateways = nil
	n.mu.Unlock()
	close(monitor.release)
	monitor = n.nextWait(t, cfg.NICCheckInterval)
	if len(n.discoveryHistory()) != 2 || r.Snapshot().Phase != PhaseConnected {
		t.Fatal("healthy session was rediscovered or disconnected because UDP stopped replying")
	}
	cancel()
	if err := awaitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ended(t, monitor.done)
	if s := r.Snapshot(); s.Phase != PhaseStopped || s.RTTValid {
		t.Fatalf("bad stopped snapshot: %+v", s)
	}
	if err := r.WithSession(t.Context(), func(context.Context, uint64, *session.Session) error { t.Fatal("stopped callback called"); return nil }); !errors.Is(err, ErrNotConnected) {
		t.Fatal(err)
	}
}

func TestClientAddressChangesEndOldGeneration(t *testing.T) {
	for _, change := range []string{"address", "prefix", "missing", "fixed-address"} {
		t.Run(change, func(t *testing.T) {
			nic, gateway := gatewayState(t)
			n := networkForTest(nic, gateway)
			r, cfg, states, peers, cancel, result := startClientLoop(t, n, change == "fixed-address")
			connectedGeneration(t, states, 1)
			first := takePeer(t, peers)
			monitor := n.nextWait(t, cfg.NICCheckInterval)
			callbackStarted, callbackDone := make(chan struct{}), make(chan error, 1)
			go func() {
				callbackDone <- r.WithSession(t.Context(), func(ctx context.Context, generation uint64, s *session.Session) error {
					if generation != 1 {
						return errors.New("callback borrowed wrong generation")
					}
					close(callbackStarted)
					<-ctx.Done()
					return ctx.Err()
				})
			}()
			ended(t, callbackStarted)
			changed := nic
			switch change {
			case "address":
				changed.Address = netip.MustParseAddr("127.0.0.3")
				n.setNICs(changed)
			case "prefix":
				changed.Prefix = netip.MustParsePrefix("127.0.0.0/16")
				n.setNICs(changed)
			case "missing":
				n.setNICs()
			case "fixed-address":
				changed.Address = netip.MustParseAddr("127.0.0.3")
				n.setNICs(changed)
			}
			close(monitor.release)
			if change == "missing" || change == "fixed-address" {
				snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseWaitingNIC })
				waitNIC := n.nextWait(t, cfg.NICCheckInterval)
				if len(n.discoveryHistory()) != 1 {
					t.Fatal("missing fixed source drifted to a different address")
				}
				n.setNICs(nic)
				close(waitNIC.release)
				changed = nic
			}
			got := connectedGeneration(t, states, 2)
			second := takePeer(t, peers)
			ended(t, monitor.done)
			ended(t, first.transport.Done())
			if err := awaitError(t, callbackDone); !errors.Is(err, context.Canceled) {
				t.Fatalf("old callback survived generation: %v", err)
			}
			history := n.discoveryHistory()
			if len(history) != 2 || history[1] != changed || got.NIC != changed {
				t.Fatalf("stale rediscovery: history=%+v snapshot=%+v", history, got)
			}
			if first.session == second.session {
				t.Fatal("session reused across generations")
			}
			cancel()
			if err := awaitError(t, result); !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			ended(t, second.transport.Done())
		})
	}
}

func TestClientRejectsAmbiguity(t *testing.T) {
	for _, when := range []string{"interface-before", "gateway", "interface-during"} {
		t.Run(when, func(t *testing.T) {
			nic, gateway := gatewayState(t)
			n := networkForTest(nic, gateway)
			other := nic
			other.Address = netip.MustParseAddr("127.0.0.3")
			if when == "interface-before" {
				n.nics = append(n.nics, other)
			}
			if when == "gateway" {
				n.gateways = append(n.gateways, gateway)
			}
			r, cfg, states, peers, _, result := startClientLoop(t, n, false)
			if when == "interface-during" {
				connectedGeneration(t, states, 1)
				first := takePeer(t, peers)
				monitor := n.nextWait(t, cfg.NICCheckInterval)
				n.setNICs(nic, other)
				close(monitor.release)
				ended(t, first.transport.Done())
			}
			select {
			case err := <-result:
				if err == nil || when != "gateway" && !errors.Is(err, carside.ErrNICAmbiguous) {
					t.Fatalf("ambiguity accepted: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("ambiguous selection retried")
			}
			if r.Snapshot().Phase != PhaseStopped {
				t.Fatal("ambiguous runtime did not stop")
			}
		})
	}
}

func TestClientRetryIsBoundedAndCancelable(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	store, dir := storeForTest(t)
	cfg := ClientConfig{StateDir: dir, Peer: identity.Fingerprint{2}, NICName: nic.Name, RetryMin: time.Millisecond, RetryMax: 4 * time.Millisecond, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: n.interfaces, Discover: n.discover, Wait: n.wait}}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var calls atomic.Int32
	fail := errors.New("test network failure")
	connector := dialFunc(func(context.Context, transport.PeerInfo) (transport.Session, error) { calls.Add(1); return nil, fail })
	r := New()
	states := r.Subscribe(ctx)
	done := make(chan error, 1)
	go func() { done <- r.clientLoop(ctx, cfg, connector, transport.PeerInfo{ID: cfg.Peer}, [16]byte{}, store) }()
	for _, d := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond} {
		got := snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseBackoff })
		if got.NextRetry.IsZero() || got.LastError != fail.Error() {
			t.Fatalf("missing retry state: %+v", got)
		}
		w := n.nextWait(t, d)
		if calls.Load() < 4 {
			close(w.release)
		} else {
			cancel()
			ended(t, w.done)
		}
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry ignored cancellation")
	}
	if calls.Load() != 4 {
		t.Fatal("retry continued after cancellation")
	}
}

func TestClientPairingAndVersionFailuresDoNotRetry(t *testing.T) {
	for _, failure := range []error{session.ErrPairing, session.ErrVersion} {
		t.Run(failure.Error(), func(t *testing.T) {
			nic, gateway := gatewayState(t)
			n := networkForTest(nic, gateway)
			store, dir := storeForTest(t)
			cfg := ClientConfig{StateDir: dir, Peer: identity.Fingerprint{2}, NICName: nic.Name, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: n.interfaces, Discover: n.discover, Wait: n.wait}}
			if err := cfg.defaults(); err != nil {
				t.Fatal(err)
			}
			connector := dialFunc(func(context.Context, transport.PeerInfo) (transport.Session, error) { return nil, failure })
			r := New()
			if err := r.clientLoop(t.Context(), cfg, connector, transport.PeerInfo{ID: cfg.Peer}, [16]byte{}, store); !errors.Is(err, failure) {
				t.Fatal(err)
			}
			if len(n.discoveryHistory()) != 1 {
				t.Fatal("fatal error retried")
			}
		})
	}
}

// Hold actual carside TCP connections across a source change. Their bytes are
// opaque; only an explicit new stream can create a new vehicle connection.
func TestClientChangeClosesVehicleTCPWithoutReplay(t *testing.T) {
	nic, gateway := gatewayState(t)
	ln, err := net.ListenTCP("tcp4", net.TCPAddrFromAddrPort(netip.AddrPortFrom(gateway.Address, 0)))
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	backendDone := make(chan struct{}, 3)
	var accepts atomic.Int32
	var wg sync.WaitGroup
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		for {
			conn, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			accepts.Add(1)
			wg.Add(1)
			go func() { defer wg.Done(); defer conn.Close(); _, _ = io.Copy(conn, conn); backendDone <- struct{}{} }()
		}
	}()
	t.Cleanup(func() { ln.Close(); ended(t, acceptDone); wg.Wait() })
	n := networkForTest(nic, gateway)
	_, cfg, states, peers, cancel, result := startClientLoop(t, n, false)
	connectedGeneration(t, states, 1)
	first := takePeer(t, peers)
	streams := make([]transport.Stream, 2)
	for i := range streams {
		streams[i], err = first.session.OpenTunnel(t.Context(), proto.OpenTunnel{Kind: "tcp", Target: ln.Addr().String()})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := streams[i].Write([]byte("opaque")); err != nil {
			t.Fatal(err)
		}
		b := make([]byte, 6)
		if _, err := io.ReadFull(streams[i], b); err != nil || string(b) != "opaque" {
			t.Fatalf("tunnel echo: %q %v", b, err)
		}
	}
	monitor := n.nextWait(t, cfg.NICCheckInterval)
	changed := nic
	changed.Prefix = netip.MustParsePrefix("127.0.0.0/16")
	n.setNICs(changed)
	close(monitor.release)
	connectedGeneration(t, states, 2)
	_ = takePeer(t, peers)
	for range streams {
		select {
		case <-backendDone:
		case <-time.After(3 * time.Second):
			t.Fatal("vehicle TCP survived source change")
		}
	}
	ended(t, first.transport.Done())
	if accepts.Load() != 2 {
		t.Fatal("old vehicle requests were replayed")
	}
	for _, st := range streams {
		st.Close()
	}
	cancel()
	if err := awaitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestEnumerationFailureWaitsWithoutDisconnectingHealthySession(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	n.enumErr = errors.New("temporary interface query failure")
	r, cfg, states, peers, cancel, result := startClientLoop(t, n, false)
	snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseBackoff })
	retry := n.nextWait(t, cfg.RetryMin)
	n.mu.Lock()
	n.enumErr = nil
	n.mu.Unlock()
	close(retry.release)
	connectedGeneration(t, states, 1)
	_ = takePeer(t, peers)
	monitor := n.nextWait(t, cfg.NICCheckInterval)
	n.mu.Lock()
	n.enumErr = errors.New("temporary interface query failure")
	n.mu.Unlock()
	close(monitor.release)
	monitor = n.nextWait(t, cfg.NICCheckInterval)
	if s := r.Snapshot(); s.Phase != PhaseConnected || s.Generation != 1 {
		t.Fatalf("uncertain enumeration disconnected healthy session: %+v", s)
	}
	cancel()
	if err := awaitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ended(t, monitor.done)
}

func TestDiscoveryCannotDialStaleNIC(t *testing.T) {
	nic, gateway := gatewayState(t)
	changed := nic
	changed.Prefix = netip.MustParsePrefix("127.0.0.0/16")
	n := networkForTest(nic, gateway)
	store, dir := storeForTest(t)
	cfg := ClientConfig{StateDir: dir, Peer: identity.Fingerprint{2}, NICName: nic.Name, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: n.interfaces, Wait: n.wait}}
	cfg.Hooks.Discover = func(ctx context.Context, o carside.DiscoverOptions) ([]carside.Gateway, error) {
		gateways, err := n.discover(ctx, o)
		n.setNICs(changed)
		return gateways, err
	}
	if err := cfg.defaults(); err != nil {
		t.Fatal(err)
	}
	var dials int
	connector := dialFunc(func(context.Context, transport.PeerInfo) (transport.Session, error) {
		dials++
		return nil, session.ErrPairing
	})
	r := New()
	if err := r.clientLoop(t.Context(), cfg, connector, transport.PeerInfo{ID: cfg.Peer}, [16]byte{}, store); !errors.Is(err, session.ErrPairing) {
		t.Fatal(err)
	}
	history := n.discoveryHistory()
	if dials != 1 || len(history) != 2 || history[0] != nic || history[1] != changed {
		t.Fatalf("stale source dialed: dials=%d discoveries=%+v", dials, history)
	}
}

func awaitError(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(3 * time.Second):
		t.Fatal("operation did not terminate")
		return nil
	}
}

func TestReconnectWaitAdvancesAfterTransientPeerBusy(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	retrying := make(chan time.Duration, 1)
	allowRetry := make(chan struct{})
	var dials atomic.Int32
	var first testPeer
	var firstLocal transport.Session
	wait := n.manualNICWait(time.Hour)
	options := clientLoopOptions{
		connector: func(next transport.Connector) transport.Connector {
			return dialFunc(func(ctx context.Context, peer transport.PeerInfo) (transport.Session, error) {
				attempt := dials.Add(1)
				if attempt == 2 {
					select {
					case <-firstLocal.Done():
					default:
						t.Error("redial started before old transport closed")
					}
					return nil, hubtransport.ErrPeerBusy
				}
				ts, err := next.Dial(ctx, peer)
				if attempt == 1 {
					firstLocal = ts
				}
				return ts, err
			})
		},
		wait: func(ctx context.Context, delay time.Duration) error {
			if delay != time.Hour {
				select {
				case retrying <- delay:
				case <-ctx.Done():
					return ctx.Err()
				}
				select {
				case <-allowRetry:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return wait(ctx, delay)
		},
	}
	r, cfg, states, peers, cancel, result := startClientLoop(t, n, false, options)
	connectedGeneration(t, states, 1)
	first = takePeer(t, peers)
	monitor := n.nextWait(t, cfg.NICCheckInterval)
	nic.Prefix = netip.MustParsePrefix("127.0.0.0/16")
	n.setNICs(nic)
	close(monitor.release)
	select {
	case delay := <-retrying:
		if delay != cfg.RetryMin {
			t.Fatalf("retry duration=%s, want %s", delay, cfg.RetryMin)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("injected busy did not enter backoff")
	}
	if s := r.Snapshot(); s.Phase != PhaseBackoff || s.Generation != 1 || s.LastError != hubtransport.ErrPeerBusy.Error() || s.NextRetry.IsZero() {
		t.Fatalf("busy was bypassed rather than retried: %+v", s)
	}
	retryStarted := time.Now()
	close(allowRetry)
	second := connectedGeneration(t, states, 2)
	if time.Since(retryStarted) < cfg.RetryMin {
		t.Fatal("network backoff was skipped")
	}
	_ = takePeer(t, peers)
	if second.NIC != nic || dials.Load() != 3 {
		t.Fatalf("unexpected retry result: snapshot=%+v dials=%d", second, dials.Load())
	}
	ended(t, monitor.done)
	ended(t, first.transport.Done())
	cancel()
	if err := awaitError(t, result); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestNextWaitSkipsCanceledQueuedRequest(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	oldCtx, cancelOld := context.WithCancel(t.Context())
	defer cancelOld()
	oldResult := make(chan error, 1)
	go func() { oldResult <- n.wait(oldCtx, time.Hour) }()
	var old waitRequest
	select {
	case old = <-n.waits:
	case <-time.After(3 * time.Second):
		t.Fatal("old wait was not queued")
	}
	n.waits <- old
	cancelOld()
	if err := awaitError(t, oldResult); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ended(t, old.done)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	currentResult := make(chan error, 1)
	go func() { currentResult <- n.wait(ctx, time.Hour) }()
	current := n.nextWait(t, time.Hour)
	if current.release == old.release || current.ctx.Err() != nil {
		t.Fatal("canceled queued request was returned as current")
	}
	close(current.release)
	if err := awaitError(t, currentResult); err != nil {
		t.Fatal(err)
	}
}

func TestManualNICWaitCancelsNetworkTimer(t *testing.T) {
	nic, gateway := gatewayState(t)
	n := networkForTest(nic, gateway)
	wait := n.manualNICWait(time.Hour)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- wait(ctx, time.Hour/2) }()
	cancel()
	if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("network timer ignored cancellation: %v", err)
	}
	if len(n.waits) != 0 {
		t.Fatal("network retry entered the manual NIC queue")
	}
}
