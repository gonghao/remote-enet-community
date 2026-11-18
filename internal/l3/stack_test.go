package l3

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

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"

	"remote-enet/internal/identity"
	"remote-enet/internal/packetio"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunnel"
)

type openerFunc func(context.Context, proto.OpenTunnel) (transport.Stream, error)

func (f openerFunc) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	return f(ctx, h)
}

var target = netip.MustParseAddr("192.0.2.10")
var source = netip.MustParseAddr("192.0.2.1")

type fixture struct {
	ctx         context.Context
	s           *Stack
	client      *stack.Stack
	device      packetio.Device
	dropInbound atomic.Bool
}

func setup(t *testing.T, opts Options) *fixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	a, b, err := packetio.NewPair(1500, 32)
	if err != nil {
		t.Fatal(err)
	}
	opts.Targets = []netip.Addr{target}
	s, err := Start(ctx, a, opts)
	if err != nil {
		t.Fatal(err)
	}
	client := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	link := channel.New(32, 1500, "")
	if err := client.CreateNIC(1, link); err != nil {
		t.Fatal(err)
	}
	if err := client.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4(source.As4()).WithPrefix()}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	f := &fixture{ctx: ctx, s: s, client: client, device: a}
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		buf := make([]byte, 1500)
		for {
			n, err := b.ReadPacket(ctx, buf)
			if err != nil {
				return
			}
			if f.dropInbound.Load() {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
			link.InjectInbound(ipv4.ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	go func() {
		defer workers.Done()
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			_, err := b.WritePacket(ctx, v.AsSlice())
			v.Release()
			pkt.DecRef()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		_ = s.Close()
		cancel()
		workers.Wait()
		link.Close()
		client.Destroy()
		_ = a.Close()
	})
	return f
}

func (f *fixture) dial(t *testing.T, addr netip.Addr, port uint16) *gonet.TCPConn {
	t.Helper()
	c, err := gonet.DialContextTCP(f.ctx, f.client, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(addr.As4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_ = c.SetDeadline(time.Now().Add(3 * time.Second))
	return c
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSessionTCPHalfClose(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	headers := make(chan proto.OpenTunnel, 1)
	serverErr := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, b, session.Options{Role: session.RoleClient, Trusted: func(identity.Fingerprint) bool { return true }, OnTunnel: func(ctx context.Context, h proto.OpenTunnel, s transport.Stream) {
			defer s.Close()
			headers <- h
			data, err := io.ReadAll(s)
			if err != nil {
				return
			}
			_, _ = s.Write(append([]byte("reply:"), data...))
			_ = s.CloseWrite()
		}})
		serverErr <- err
	}()
	operator, err := session.Run(ctx, a, session.Options{Role: session.RoleOperator, Initiator: true, Trusted: func(identity.Fingerprint) bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	activity := &observedActivity{}
	var releases atomic.Int32
	var admitted atomic.Bool
	f := setup(t, Options{Observer: activity, Opener: operator, Admit: func(ctx context.Context, dst, src netip.AddrPort) (func(), error) {
		if dst != netip.AddrPortFrom(target, 22) || src.Addr() != source {
			t.Errorf("wrong admission endpoints %s %s", dst, src)
		}
		admitted.Store(true)
		return func() { releases.Add(1) }, nil
	}})
	c := f.dial(t, target, 22)
	if _, err := c.Write([]byte("request")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	reply, err := io.ReadAll(c)
	if err != nil || string(reply) != "reply:request" {
		t.Fatalf("reply %q: %v", reply, err)
	}
	h := <-headers
	if h.Kind != "tcp" || h.Target != "192.0.2.10:22" || h.Origin != c.LocalAddr().String() {
		t.Fatalf("header %+v, origin %s", h, c.LocalAddr())
	}
	eventually(t, func() bool { return releases.Load() == 1 && f.s.Snapshot().Active == 0 })
	if activity.in.Load() != 13 || activity.out.Load() != 7 || activity.ends.Load() != 1 {
		t.Fatalf("observer in=%d out=%d ends=%d", activity.in.Load(), activity.out.Load(), activity.ends.Load())
	}
	if !admitted.Load() || f.s.Snapshot().Accepted != 1 {
		t.Fatal(f.s.Snapshot())
	}
}

func TestBudgetAndCancellation(t *testing.T) {
	opened := make(chan struct{}, 4)
	var released atomic.Int32
	f := setup(t, Options{MaxConnections: 1, Opener: openerFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
		opened <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}), Admit: func(context.Context, netip.AddrPort, netip.AddrPort) (func(), error) {
		return func() { released.Add(1) }, nil
	}})
	_ = f.dial(t, target, 22)
	<-opened
	ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, f.client, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(target.As4()), Port: 80}, ipv4.ProtocolNumber)
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("over-budget connection succeeded")
	}
	if snap := f.s.Snapshot(); snap.Active != 1 || snap.Rejected == 0 {
		t.Fatal(snap)
	}
	done := make(chan struct{})
	go func() { _ = f.s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not join opener")
	}
	if released.Load() != 1 || f.s.Snapshot().Active != 0 {
		t.Fatal("reservation was not released")
	}
	// Close must not close the caller's packet device.
	writeCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if n, err := f.device.WritePacket(writeCtx, []byte{0}); err != nil || n != 1 {
		t.Fatalf("device closed by stack: %d %v", n, err)
	}
}

func TestAdmissionAndOpenFailuresRelease(t *testing.T) {
	for _, deny := range []bool{true, false} {
		t.Run(map[bool]string{true: "admission", false: "opener"}[deny], func(t *testing.T) {
			var releases, opens atomic.Int32
			activity := &observedActivity{}
			f := setup(t, Options{Observer: activity, Opener: openerFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
				opens.Add(1)
				return nil, errors.New("open failed")
			}), Admit: func(context.Context, netip.AddrPort, netip.AddrPort) (func(), error) {
				var err error
				if deny {
					err = errors.New("denied")
				}
				return func() { releases.Add(1) }, err
			}})
			for range 2 {
				c := f.dial(t, target, 22)
				_, _ = io.ReadAll(c)
				eventually(t, func() bool { return f.s.Snapshot().Active == 0 })
			}
			if activity.ends.Load() != 2 || activity.failed.Load() != 2 || activity.in.Load() != 0 || activity.out.Load() != 0 {
				t.Fatal("failure observation missing or duplicated")
			}
			if releases.Load() != 2 || f.s.Snapshot().Rejected != 2 || f.s.Snapshot().Accepted != 0 {
				t.Fatal(releases.Load(), f.s.Snapshot())
			}
			if (deny && opens.Load() != 0) || (!deny && opens.Load() != 2) {
				t.Fatal(opens.Load())
			}
		})
	}
}

func TestWhitelistAndUnknownProtocols(t *testing.T) {
	var opens atomic.Int32
	f := setup(t, Options{Opener: openerFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		opens.Add(1)
		return nil, errors.New("unexpected open")
	})})
	ctx, cancel := context.WithTimeout(f.ctx, 100*time.Millisecond)
	defer cancel()
	c, err := gonet.DialContextTCP(ctx, f.client, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{192, 0, 2, 11}), Port: 22}, ipv4.ProtocolNumber)
	if c != nil {
		_ = c.Close()
	}
	if err == nil {
		t.Fatal("nonallowlisted target connected")
	}
	// Inject IPv4 UDP and IPv6 into the same real device path.
	pkt := make([]byte, 40)
	pkt[0] = 0x45
	pkt[9] = 17
	copy(pkt[16:20], target.AsSlice())
	// The stack-side device writes toward the client, so use a dedicated pair.
	a, b, _ := packetio.NewPair(1500, 4)
	s, err := Start(f.ctx, a, Options{Targets: []netip.Addr{target}, Opener: f.s.opts.Opener})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defer s.Close()
	_, _ = b.WritePacket(f.ctx, pkt)
	pkt[0] = 0x60
	_, _ = b.WritePacket(f.ctx, pkt)
	eventually(t, func() bool { return s.Snapshot().Rejected == 2 })
	if opens.Load() != 0 || f.s.Snapshot().Rejected == 0 {
		t.Fatal("unsupported traffic reached opener")
	}
}

func TestCloseJoinsHalfOpenHandshake(t *testing.T) {
	f := setup(t, Options{MaxConnections: 1, Opener: openerFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		t.Error("half-open handshake reached opener")
		return nil, errors.New("unexpected open")
	})})
	f.dropInbound.Store(true)
	ctx, cancel := context.WithCancel(f.ctx)
	dialDone := make(chan struct{})
	go func() {
		defer close(dialDone)
		c, _ := gonet.DialContextTCP(ctx, f.client, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(target.As4()), Port: 22}, ipv4.ProtocolNumber)
		if c != nil {
			_ = c.Close()
		}
	}()
	eventually(t, func() bool { return f.s.Snapshot().Active == 1 })
	done := make(chan struct{})
	go func() { _ = f.s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close left half-open handshake running")
	}
	cancel()
	<-dialDone
	if f.s.Snapshot().Active != 0 {
		t.Fatal(f.s.Snapshot())
	}
}

func TestInvalidOptions(t *testing.T) {
	a, _, _ := packetio.NewPair(1500, 1)
	defer a.Close()
	opener := openerFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return nil, errors.New("unused") })
	for _, opts := range []Options{
		{Opener: opener},
		{Targets: []netip.Addr{target}},
		{Targets: []netip.Addr{target}, Opener: opener, MaxConnections: -1},
		{Targets: []netip.Addr{target}, Opener: opener, MaxConnections: 65536},
		{Targets: []netip.Addr{target, target}, Opener: opener},
		{Targets: []netip.Addr{netip.MustParseAddr("::ffff:192.0.2.10")}, Opener: opener},
		{Targets: []netip.Addr{netip.MustParseAddr("127.0.0.1")}, Opener: opener},
	} {
		s, err := Start(context.Background(), a, opts)
		if err == nil {
			_ = s.Close()
			t.Fatal("invalid options accepted")
		}
	}
}

func TestFailureIsolationAndCloseJoinsPipe(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(c, c)
	}()
	var releases atomic.Int32
	f := setup(t, Options{MaxConnections: 2, Opener: openerFunc(func(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
		if h.Target == "192.0.2.10:22" {
			return nil, errors.New("one destination failed")
		}
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp4", ln.Addr().String())
		if err != nil {
			return nil, err
		}
		return c.(*net.TCPConn), nil
	}), Admit: func(context.Context, netip.AddrPort, netip.AddrPort) (func(), error) {
		return func() { releases.Add(1) }, nil
	}})
	c := f.dial(t, target, 80)
	eventually(t, func() bool { return f.s.Snapshot().Accepted == 1 })
	bad := f.dial(t, target, 22)
	_, _ = io.ReadAll(bad)
	eventually(t, func() bool { return releases.Load() == 1 })
	if _, err := c.Write([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(c, data); err != nil || string(data) != "alive" {
		t.Fatalf("healthy flow affected: %q %v", data, err)
	}
	done := make(chan struct{})
	go func() { _ = f.s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Close did not join active pipe")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("loopback connection remained open")
	}
	if releases.Load() != 2 || f.s.Snapshot().Active != 0 {
		t.Fatal(releases.Load(), f.s.Snapshot())
	}
}

type observedActivity struct {
	in, out      atomic.Uint64
	ends, failed atomic.Int32
}

func (a *observedActivity) Begin(string) tunnel.Activity { return a }
func (a *observedActivity) AddIn(n uint64)               { a.in.Add(n) }
func (a *observedActivity) AddOut(n uint64)              { a.out.Add(n) }
func (a *observedActivity) End(err error) {
	if err != nil {
		a.failed.Add(1)
	}
	a.ends.Add(1)
}

type failingDevice struct {
	packetio.Device
	writeErr error
	short    bool
}

func (d *failingDevice) WritePacket(context.Context, []byte) (int, error) {
	if d.short {
		return 0, nil
	}
	return 0, d.writeErr
}

func TestDeviceFailureAndNormalClose(t *testing.T) {
	for _, mode := range []string{"read", "write", "short write", "normal"} {
		t.Run(mode, func(t *testing.T) {
			a, b, err := packetio.NewPair(1500, 8)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			defer b.Close()
			failure := errors.New("device disconnected")
			device := packetio.Device(a)
			if mode == "write" || mode == "short write" {
				device = &failingDevice{Device: a, writeErr: failure, short: mode == "short write"}
			}
			s, err := Start(t.Context(), device, Options{Targets: []netip.Addr{target}, Opener: openerFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) { return nil, errors.New("unused") })})
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			switch mode {
			case "read":
				_ = a.Close()
			case "normal":
				_ = s.Close()
			default:
				pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData([]byte{1, 2, 3})})
				// Queue an outbound packet without requiring a TCP handshake.
				var packets stack.PacketBufferList
				packets.PushBack(pkt)
				s.link.WritePackets(packets)
				pkt.DecRef()
			}
			select {
			case <-s.Done():
			case <-time.After(time.Second):
				t.Fatal("shutdown not signaled")
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if mode == "normal" {
				if s.Err() != nil {
					t.Fatal(s.Err())
				}
			} else if s.Err() == nil {
				t.Fatal("missing failure")
			}
			if mode == "write" && !errors.Is(s.Err(), failure) {
				t.Fatal(s.Err())
			}
			if mode == "short write" && !errors.Is(s.Err(), io.ErrShortWrite) {
				t.Fatal(s.Err())
			}
		})
	}
}
