package operator

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

type openFunc func(context.Context, proto.OpenTunnel) (transport.Stream, error)

func (f openFunc) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	return f(ctx, h)
}

func testState(t *testing.T, address string) proto.VehicleState {
	t.Helper()
	id := hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, VIN: "WBAXXXXXXX0000001"}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: append(id.MarshalBody(), []byte("EXTENSION\x00\xff")...)}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return proto.VehicleState{GatewayAddr: address, VIN: id.VIN, IdentRaw: raw}
}

func startPresenter(t *testing.T, o Options) *Presenter {
	t.Helper()
	if !o.Bind.IsValid() {
		o.Bind = netip.MustParseAddr("127.0.0.1")
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	p, err := Start(t.Context(), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

func udpClient(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func probe(t *testing.T, c *net.UDPConn, target *net.UDPAddr, request []byte, want []byte) {
	t.Helper()
	if _, err := c.WriteToUDP(request, target); err != nil {
		t.Fatal(err)
	}
	deadline := time.Second
	if want == nil {
		deadline = 30 * time.Millisecond
	}
	if err := c.SetReadDeadline(time.Now().Add(deadline)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 65535)
	n, from, err := c.ReadFromUDP(buf)
	if want == nil {
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("expected no discovery response, got %x: %v", buf[:n], err)
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buf[:n], want) || !from.IP.Equal(target.IP) || from.Port != target.Port {
		t.Fatalf("discovery response from %v: %x", from, buf[:n])
	}
}

func TestDiscoveryValidationAndSnapshots(t *testing.T) {
	opens := 0
	opener := openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		opens++
		return nil, errors.New("unexpected open")
	})
	p := startPresenter(t, Options{Opener: opener})
	_, _, disc := p.Addr()
	c := udpClient(t)
	probe(t, c, disc, hsfz.IdentRequest, nil)
	state := testState(t, "127.0.0.2")
	want := bytes.Clone(state.IdentRaw)
	if err := p.Update(state); err != nil {
		t.Fatal(err)
	}
	state.IdentRaw[6] ^= 0xff
	probe(t, c, disc, hsfz.IdentRequest, want)
	for _, invalid := range [][]byte{nil, {0}, {0, 0, 0, 0, 0, 0x10}, append(bytes.Clone(hsfz.IdentRequest), 0)} {
		probe(t, c, disc, invalid, nil)
	}
	invalid := testState(t, "127.0.0.3")
	invalid.IdentRaw = append(invalid.IdentRaw, 0)
	if err := p.Update(invalid); err == nil {
		t.Fatal("trailing bytes accepted")
	}
	probe(t, c, disc, hsfz.IdentRequest, want)
	if err := p.Update(proto.VehicleState{}); err != nil {
		t.Fatal(err)
	}
	probe(t, c, disc, hsfz.IdentRequest, nil)
	if opens != 0 {
		t.Fatal("discovery opened a data tunnel")
	}
}

func newEchoOpener(t *testing.T) (Opener, <-chan proto.OpenTunnel, transport.Session) {
	t.Helper()
	local, remote := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	ctx, cancel := context.WithCancel(t.Context())
	requests := make(chan proto.OpenTunnel, 64)
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		for {
			st, err := remote.AcceptStream(ctx)
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer st.Close()
				body, err := io.ReadAll(st)
				if err == nil {
					_, err = st.Write(body)
				}
				if err == nil {
					_ = st.CloseWrite()
				}
			}()
		}
	}()
	t.Cleanup(func() { cancel(); local.Close(nil); remote.Close(nil); workers.Wait() })
	return openFunc(func(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
		select {
		case requests <- h:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return local.OpenStream(ctx)
	}), requests, local
}

func dialTCP(t *testing.T, addr *net.TCPAddr) *net.TCPConn {
	t.Helper()
	c, err := net.DialTCP("tcp4", nil, addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func nextRequest(t *testing.T, requests <-chan proto.OpenTunnel) proto.OpenTunnel {
	t.Helper()
	select {
	case h := <-requests:
		return h
	case <-time.After(time.Second):
		t.Fatal("tunnel open timeout")
		return proto.OpenTunnel{}
	}
}

func roundTrip(t *testing.T, c *net.TCPConn, want []byte) {
	t.Helper()
	if _, err := c.Write(want); err != nil {
		t.Error(err)
		return
	}
	if err := c.CloseWrite(); err != nil {
		t.Error(err)
		return
	}
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, want) {
		t.Errorf("half-close reply: bytes=%d err=%v", len(got), err)
	}
}

func TestBothTCPChannelsAndConcurrentHalfClose(t *testing.T) {
	opener, requests, ts := newEchoOpener(t)
	p := startPresenter(t, Options{State: testState(t, "127.0.0.2"), Opener: opener, RemoteDiagnosticPort: 20001, RemoteControlPort: 20002})
	diag, ctrl, _ := p.Addr()
	var wg sync.WaitGroup
	for i := range 12 {
		addr, target := diag, "127.0.0.2:20001"
		if i%2 == 1 {
			addr, target = ctrl, "127.0.0.2:20002"
		}
		c := dialTCP(t, addr)
		h := nextRequest(t, requests)
		if h.Kind != "tcp" || h.Target != target || h.Origin != c.LocalAddr().String() {
			t.Fatalf("wrong tunnel header: %+v", h)
		}
		wg.Go(func() { roundTrip(t, c, bytes.Repeat([]byte{0, 0xff, 0x12, 0x34}, 80000)) })
	}
	wg.Wait()
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ts.Done():
		t.Fatal("presenter closed its upstream session")
	default:
	}
}

func TestUpdateOnlyRetargetsNewConnections(t *testing.T) {
	opener, requests, _ := newEchoOpener(t)
	state := testState(t, "127.0.0.2")
	wantRaw := bytes.Clone(state.IdentRaw)
	p := startPresenter(t, Options{State: state, Opener: opener})
	state.IdentRaw[6] ^= 0xff
	diag, ctrl, disc := p.Addr()
	probe(t, udpClient(t), disc, hsfz.IdentRequest, wantRaw)
	old := dialTCP(t, diag)
	if h := nextRequest(t, requests); h.Target != "127.0.0.2:6801" {
		t.Fatal(h)
	}
	if err := p.Update(testState(t, "127.0.0.3")); err != nil {
		t.Fatal(err)
	}
	current := dialTCP(t, ctrl)
	if h := nextRequest(t, requests); h.Target != "127.0.0.3:6811" {
		t.Fatal(h)
	}
	roundTrip(t, old, []byte("old connection remains valid"))
	roundTrip(t, current, []byte("new connection sees update"))
}

func TestEmptyStateRejectsTCPAndFailedOpenClosesClient(t *testing.T) {
	requests := make(chan proto.OpenTunnel, 1)
	p := startPresenter(t, Options{Opener: openFunc(func(_ context.Context, h proto.OpenTunnel) (transport.Stream, error) {
		requests <- h
		return nil, errors.New("unavailable")
	})})
	diag, _, _ := p.Addr()
	for _, ready := range []bool{false, true} {
		if ready {
			if err := p.Update(testState(t, "127.0.0.2")); err != nil {
				t.Fatal(err)
			}
		}
		c := dialTCP(t, diag)
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("connection survived without a tunnel")
		}
		if ready {
			nextRequest(t, requests)
		} else {
			select {
			case <-requests:
				t.Fatal("empty state opened a tunnel")
			default:
			}
		}
	}
}

func TestCloseCancelsBlockedOpenAndContextClosesCopies(t *testing.T) {
	started := make(chan struct{})
	exited := make(chan struct{})
	p := startPresenter(t, Options{State: testState(t, "127.0.0.2"), Opener: openFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
		close(started)
		<-ctx.Done()
		close(exited)
		return nil, ctx.Err()
	})})
	diag, _, _ := p.Addr()
	c := dialTCP(t, diag)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("open did not start")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	default:
		t.Fatal("Close returned before open worker exited")
	}
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("client survived Close")
	}
	if err := p.Update(proto.VehicleState{}); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	opener, requests, ts := newEchoOpener(t)
	ctx, cancel := context.WithCancel(t.Context())
	q, err := Start(ctx, Options{Bind: netip.MustParseAddr("127.0.0.1"), State: testState(t, "127.0.0.2"), Opener: opener})
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	diag, _, _ = q.Addr()
	c = dialTCP(t, diag)
	nextRequest(t, requests)
	cancel()
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("context cancellation left local connection alive")
	}
	q.Close()
	select {
	case <-ts.Done():
		t.Fatal("context cancellation closed borrowed session")
	default:
	}
}

func TestValidationAndPortConflictCleanup(t *testing.T) {
	noop := openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return nil, errors.New("unused") })
	for _, bind := range []string{"0.0.0.0", "::1", "224.0.0.1", "255.255.255.255"} {
		if p, err := Start(t.Context(), Options{Bind: netip.MustParseAddr(bind), Opener: noop}); err == nil {
			p.Close()
			t.Fatalf("invalid bind %s accepted", bind)
		}
	}
	for _, gateway := range []string{"not-an-ip", "::1", "0.0.0.0", "224.0.0.1", "255.255.255.255"} {
		if _, err := parseState(testState(t, gateway)); err == nil {
			t.Fatalf("invalid gateway %s accepted", gateway)
		}
	}
	valid := testState(t, "127.0.0.2")
	for _, raw := range [][]byte{nil, {0}, append(bytes.Clone(valid.IdentRaw), 0), {0, 0, 0, 0, 0, 0x11}, {0, 0, 0, 0, 0, 0x10}} {
		s := valid
		s.IdentRaw = raw
		if _, err := parseState(s); err == nil {
			t.Fatalf("invalid raw accepted: %x", raw)
		}
	}
	for _, port := range []int{-1, 65536} {
		if p, err := Start(t.Context(), Options{Bind: netip.MustParseAddr("127.0.0.1"), Ports: Ports{Diagnostic: port}, Opener: noop}); err == nil {
			p.Close()
			t.Fatal("invalid port accepted")
		}
	}
	if p, err := Start(t.Context(), Options{Bind: netip.MustParseAddr("127.0.0.1")}); err == nil {
		p.Close()
		t.Fatal("nil opener accepted")
	}
	for _, udpConflict := range []bool{false, true} {
		t.Run(fmt.Sprintf("udp=%t", udpConflict), func(t *testing.T) {
			diag, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer diag.Close()
			ctrl, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			defer ctrl.Close()
			disc := udpClient(t)
			ports := Ports{Diagnostic: diag.Addr().(*net.TCPAddr).Port, Control: ctrl.Addr().(*net.TCPAddr).Port, Discovery: disc.LocalAddr().(*net.UDPAddr).Port}
			diag.Close()
			if udpConflict {
				ctrl.Close()
			}
			if p, err := Start(t.Context(), Options{Bind: netip.MustParseAddr("127.0.0.1"), Ports: ports, Opener: noop}); err == nil {
				p.Close()
				t.Fatal("occupied port accepted")
			}
			for _, port := range []int{ports.Diagnostic, ports.Control} {
				if port == ports.Control && !udpConflict {
					continue
				}
				ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
				if err != nil {
					t.Fatalf("listener %d leaked: %v", port, err)
				}
				ln.Close()
			}
		})
	}
}

func TestMaintenanceRejectsToolsAndJoins(t *testing.T) {
	opened := make(chan struct{}, 1)
	p := startPresenter(t, Options{State: testState(t, "127.0.0.2"), Opener: openFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
		opened <- struct{}{}
		return nil, errors.New("unexpected open")
	})})
	entered, canceled, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		finished <- p.WithMaintenance(t.Context(), func(ctx context.Context, gateway netip.Addr) error {
			if gateway != netip.MustParseAddr("127.0.0.2") {
				return errors.New("wrong gateway")
			}
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return ctx.Err()
		})
	}()
	<-entered
	if err := p.Update(testState(t, "127.0.0.3")); err != nil {
		t.Fatal(err)
	}
	if err := p.WithMaintenance(t.Context(), func(context.Context, netip.Addr) error { return nil }); !errors.Is(err, exclusive.ErrBusy) {
		t.Fatalf("second maintenance: %v", err)
	}
	diag, _, _ := p.Addr()
	conn, err := net.Dial("tcp4", diag.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	var buf [1]byte
	if _, err = conn.Read(buf[:]); err == nil {
		t.Fatal("maintenance accepted tool")
	}
	conn.Close()
	select {
	case <-opened:
		t.Fatal("opened tool during maintenance")
	default:
	}
	closed := make(chan struct{})
	go func() { p.Close(); close(closed) }()
	<-canceled
	select {
	case <-closed:
		t.Fatal("Close did not join callback")
	default:
	}
	close(release)
	<-closed
	if err := <-finished; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestMaintenanceBusyWithAcceptedTool(t *testing.T) {
	opened := make(chan struct{})
	p := startPresenter(t, Options{State: testState(t, "127.0.0.2"), Opener: openFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
		close(opened)
		<-ctx.Done()
		return nil, ctx.Err()
	})})
	diag, _, _ := p.Addr()
	conn, err := net.Dial("tcp4", diag.String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-opened
	if err := p.WithMaintenance(t.Context(), func(context.Context, netip.Addr) error { t.Fatal("callback ran while busy"); return nil }); !errors.Is(err, exclusive.ErrBusy) {
		t.Fatal(err)
	}
}
