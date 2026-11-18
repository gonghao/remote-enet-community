package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
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
	"remote-enet/internal/l3"
	"remote-enet/internal/packetio"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
)

var l3FaultTarget = netip.MustParseAddr("192.0.2.10")

// Hold the synthetic client's maximum 4 MiB TCP window plus ACK headroom.
// An unconfigured 128-packet tail drop can stall the no-fault baseline.
const fixtureClientQueuePackets = 4096

type l3PacketFault struct {
	blackhole                 atomic.Bool
	packets, dropped, delayed atomic.Uint64
	enabled                   bool
	entered                   chan struct{}
	once                      sync.Once
}

func (f *l3PacketFault) pass(ctx context.Context) bool {
	n := f.packets.Add(1)
	if f.blackhole.Load() || (f.enabled && n == 17) {
		f.dropped.Add(1)
		f.once.Do(func() { close(f.entered) })
		return false
	}
	if f.enabled && n == 11 {
		f.delayed.Add(1)
		timer := time.NewTimer(2 * time.Millisecond)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

type l3BulkReceipt struct {
	n    int64
	hash []byte
	err  error
}

type observedPacketLink struct {
	*channel.Endpoint
	shortPackets atomic.Uint64
}

func newObservedPacketLink() *observedPacketLink {
	return &observedPacketLink{Endpoint: channel.New(fixtureClientQueuePackets, 1500, "")}
}

func (l *observedPacketLink) WritePackets(packets stack.PacketBufferList) (int, tcpip.Error) {
	n, err := l.Endpoint.WritePackets(packets)
	if missing := packets.Len() - n; missing > 0 {
		l.shortPackets.Add(uint64(missing))
	}
	return n, err
}

type l3FaultFixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	l3        *l3.Stack
	client    *stack.Stack
	device    packetio.Device
	ct, ot    transport.Session
	c, o      *session.Session
	faults    [2]*l3PacketFault
	headers   chan proto.OpenTunnel
	receipts  chan l3BulkReceipt
	callbacks sync.WaitGroup
	workers   sync.WaitGroup
	calls     atomic.Uint64
	closeOnce sync.Once
	link      *observedPacketLink
}

func newL3FaultFixture(t fixtureT, parent context.Context, faults bool, wrappers ...func(net.Conn) net.Conn) *l3FaultFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	f := &l3FaultFixture{ctx: ctx, cancel: cancel, headers: make(chan proto.OpenTunnel, 16), receipts: make(chan l3BulkReceipt, 16)}
	f.ct, f.ot, _ = connectPair(t, ctx, false, true, wrappers...)
	token := [16]byte{1, 2, 3}
	f.c, f.o = runSessions(t, ctx, f.ct, f.ot,
		session.Options{Role: session.RoleClient, Initiator: true, Token: token, OnTunnel: func(ctx context.Context, h proto.OpenTunnel, s transport.Stream) {
			f.callbacks.Add(1)
			defer f.callbacks.Done()
			defer s.Close()
			f.calls.Add(1)
			f.headers <- h
			if h.Target == "192.0.2.10:23" {
				return
			}
			stop := context.AfterFunc(ctx, func() { _ = s.Close() })
			defer stop()
			// Opaque echo: no IP, transport or application payload interpretation.
			hash := sha256.New()
			n, err := io.Copy(s, io.TeeReader(s, hash))
			_ = s.CloseWrite()
			f.receipts <- l3BulkReceipt{n: n, hash: hash.Sum(nil), err: err}
		}}, session.Options{Role: session.RoleOperator, AcceptToken: func(_ identity.Fingerprint, got [16]byte) bool { return got == token }})
	a, b, err := packetio.NewPair(1500, 128)
	if err != nil {
		t.Fatal(err)
	}
	f.device = a
	f.l3, err = l3.Start(ctx, a, l3.Options{Targets: []netip.Addr{l3FaultTarget}, Opener: f.o})
	if err != nil {
		t.Fatal(err)
	}
	f.client = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	f.link = newObservedPacketLink()
	link := f.link.Endpoint
	if err := f.client.CreateNIC(1, f.link); err != nil {
		t.Fatal(err)
	}
	if err := f.client.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4([4]byte{192, 0, 2, 1}).WithPrefix()}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	f.client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	for i := range f.faults {
		f.faults[i] = &l3PacketFault{enabled: faults, entered: make(chan struct{})}
	}
	f.workers.Add(2)
	go func() {
		defer f.workers.Done()
		buf := make([]byte, 1500)
		for {
			n, err := b.ReadPacket(ctx, buf)
			if err != nil {
				return
			}
			if !f.faults[0].pass(ctx) {
				continue
			}
			p := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(bytes.Clone(buf[:n]))})
			link.InjectInbound(ipv4.ProtocolNumber, p)
			p.DecRef()
		}
	}()
	go func() {
		defer f.workers.Done()
		for {
			p := link.ReadContext(ctx)
			if p == nil {
				return
			}
			v := p.ToView()
			var err error
			if f.faults[1].pass(ctx) {
				_, err = b.WritePacket(ctx, v.AsSlice())
			}
			v.Release()
			p.DecRef()
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() { f.close(t); link.Close(); f.client.Destroy() })
	return f
}

func (f *l3FaultFixture) close(t fixtureT) {
	t.Helper()
	f.closeOnce.Do(func() {
		f.cancel()
		done := make(chan struct{})
		go func() {
			_ = f.l3.Close()
			f.workers.Wait()
			<-f.c.Done()
			<-f.o.Done()
			f.callbacks.Wait()
			_ = f.device.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Fatal("L3 fixture workers did not join")
		}
		if f.l3.Snapshot().Active != 0 {
			t.Fatal(f.l3.Snapshot())
		}
		t.Logf("joined L3/session/callback/packet workers; calls=%d snapshot=%+v", f.calls.Load(), f.l3.Snapshot())
	})
}

func (f *l3FaultFixture) dial(t fixtureT, port uint16) *gonet.TCPConn {
	t.Helper()
	c, err := gonet.DialContextTCP(f.ctx, f.client, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(l3FaultTarget.As4()), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := f.ctx.Deadline()
	_ = c.SetDeadline(deadline)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (f *l3FaultFixture) bulk(t fixtureT, size int) (int64, string, string) {
	t.Helper()
	c := f.dial(t, 22)
	defer c.Close()
	want := bytes.Repeat([]byte{0, 0xff, 0x42, 0x91}, size/4)
	sent := make(chan error, 1)
	go func() {
		n, err := io.Copy(c, bytes.NewReader(want))
		if err == nil && n != int64(size) {
			err = fmt.Errorf("short upload %d", n)
		}
		if err == nil {
			err = c.CloseWrite()
		}
		sent <- err
	}()
	hash := sha256.New()
	n, err := io.Copy(hash, c)
	if err != nil {
		t.Fatalf("download bytes=%d stack=%+v calls=%d packets=%d/%d drops=%d/%d queue_short=%d: %v", n, f.l3.Snapshot(), f.calls.Load(),
			f.faults[0].packets.Load(), f.faults[1].packets.Load(), f.faults[0].dropped.Load(), f.faults[1].dropped.Load(), f.link.shortPackets.Load(), err)
	}
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	expected := sha256.Sum256(want)
	if n != int64(size) || !bytes.Equal(hash.Sum(nil), expected[:]) {
		t.Fatalf("download bytes=%d sha256=%x want=%x", n, hash.Sum(nil), expected)
	}
	if f.link.shortPackets.Load() != 0 {
		t.Fatalf("fixture client queue dropped %d unconfigured packets", f.link.shortPackets.Load())
	}
	select {
	case h := <-f.headers:
		if h.Kind != "tcp" || h.Target != "192.0.2.10:22" || h.Origin != c.LocalAddr().String() {
			t.Fatalf("header=%+v origin=%s", h, c.LocalAddr())
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	select {
	case receipt := <-f.receipts:
		if receipt.err != nil || receipt.n != int64(size) || !bytes.Equal(receipt.hash, expected[:]) {
			t.Fatalf("upload bytes=%d sha256=%x error=%v", receipt.n, receipt.hash, receipt.err)
		}
	case <-f.ctx.Done():
		t.Fatal(f.ctx.Err())
	}
	t.Logf("path=%s upload/download=%d sha256=%x", f.ot.Path().Kind, size, expected)
	return n, fmt.Sprintf("%x", hash.Sum(nil)), fmt.Sprintf("%x", expected)
}

func TestL3FaultBulk(t *testing.T) {
	for _, tc := range []struct {
		name  string
		size  int
		fault bool
	}{{"1MiB", 1 << 20, false}, {"8MiB", 8 << 20, false}, {"1MiB-IP-delay-drop", 1 << 20, true}, {"8MiB-IP-delay-drop", 8 << 20, true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			f := newL3FaultFixture(t, ctx, tc.fault)
			f.bulk(t, tc.size)
			if tc.fault {
				for i, g := range f.faults {
					if g.dropped.Load() != 1 || g.delayed.Load() != 1 {
						t.Fatalf("direction %d packets=%d drops=%d delays=%d", i, g.packets.Load(), g.dropped.Load(), g.delayed.Load())
					}
					t.Logf("IP direction=%d packets=%d drops=%d delays=%d (no stream pause)", i, g.packets.Load(), g.dropped.Load(), g.delayed.Load())
				}
			}
		})
	}
}

func TestL3ExternalNetem(t *testing.T) {
	if os.Getenv("RENET_MANUAL_L3_NETEM") != "1" {
		t.Skip("set RENET_MANUAL_L3_NETEM=1 inside the externally managed loopback netem namespace")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 110*time.Second)
	defer cancel()
	for _, size := range []int{1 << 20, 8 << 20} {
		var result struct {
			Schema         int    `json:"schema"`
			Bytes          int64  `json:"bytes"`
			SHA256         string `json:"sha256"`
			ExpectedSHA256 string `json:"expected_sha256"`
			ActiveAfter    uint64 `json:"active_after"`
			WorkersJoined  bool   `json:"workers_joined"`
			UID            int    `json:"uid"`
			GID            int    `json:"gid"`
			Path           string `json:"path"`
		}
		// Returning from Run includes connectPair's Hub and transport cleanups.
		if !t.Run(fmt.Sprintf("relay-%dMiB", size>>20), func(t *testing.T) {
			f := newL3FaultFixture(t, ctx, false)
			result.Bytes, result.SHA256, result.ExpectedSHA256 = f.bulk(t, size)
			f.close(t)
			result.ActiveAfter = f.l3.Snapshot().Active
			result.Path = string(f.ot.Path().Kind)
		}) {
			return
		}
		result.Schema = 1
		result.WorkersJoined = true
		result.UID = os.Getuid()
		result.GID = os.Getgid()
		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("L3_NETEM_RESULT %s", encoded)
	}
}

func TestL3FaultRecoveryAndIsolation(t *testing.T) {
	for _, mode := range []string{"cancel", "transport", "device"} {
		t.Run(mode, func(t *testing.T) { runL3Recovery(t, mode, 1<<20) })
	}
}

func runL3Recovery(t fixtureT, mode string, size int) (int64, string, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	f := newL3FaultFixture(t, ctx, false)
	healthy := f.dial(t, 22)
	bad := f.dial(t, 23)
	_, _ = io.ReadAll(bad)
	_ = bad.Close()
	// The failed stream must leave its established sibling usable.
	payload := []byte("isolated opaque flow")
	if _, err := healthy.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(healthy, got); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("isolated flow %q: %v", got, err)
	}
	f.faults[0].blackhole.Store(true)
	if _, err := healthy.Write([]byte("blackhole trigger")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-f.faults[0].entered:
	case <-ctx.Done():
		t.Fatal("inbound blackhole not exercised")
	}
	f.faults[1].blackhole.Store(true)
	pending := make(chan error, 1)
	go func() { _, err := io.Copy(healthy, bytes.NewReader(bytes.Repeat([]byte{0x7a}, 8<<20))); pending <- err }()
	for _, g := range f.faults {
		select {
		case <-g.entered:
		case <-ctx.Done():
			t.Fatal("bidirectional blackhole not exercised")
		}
	}
	for i, g := range f.faults {
		if g.dropped.Load() == 0 {
			t.Fatalf("blackhole direction %d did not drop packets", i)
		}
		t.Logf("blackhole direction=%d packets=%d drops=%d", i, g.packets.Load(), g.dropped.Load())
	}
	before := f.calls.Load()
	switch mode {
	case "transport":
		_ = f.ct.Close(fmt.Errorf("injected physical disconnect"))
		for _, done := range []<-chan struct{}{f.ct.Done(), f.ot.Done(), f.c.Done(), f.o.Done()} {
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("physical disconnect did not terminate sessions")
			}
		}
	case "device":
		_ = f.device.Close()
		select {
		case <-f.l3.Done():
		case <-ctx.Done():
			t.Fatal("device disconnect did not terminate stack")
		}
	case "cancel":
		f.cancel()
	}
	// Packet loss cannot deliver a remote reset; fixture cancellation owns the
	// virtual client endpoint and explicitly aborts its blocked application I/O.
	_ = healthy.Close()
	select {
	case err := <-pending:
		if err == nil {
			t.Fatal("unfinished upload unexpectedly completed")
		}
	case <-ctx.Done():
		t.Fatal("application write did not join")
	}
	f.close(t)
	if f.calls.Load() != before {
		t.Fatal("old request reopened")
	}
	// A replacement owns a fresh budget; time spent inducing and joining the
	// departed connection must not become the new connection's I/O deadline.
	rebuildCtx, stopRebuild := context.WithTimeout(t.Context(), 20*time.Second)
	defer stopRebuild()
	next := newL3FaultFixture(t, rebuildCtx, false)
	n, hash, _ := next.bulk(t, size)
	if next.calls.Load() != 1 || f.calls.Load() != before {
		t.Fatal("request replayed across generations")
	}
	return n, hash, string(next.ot.Path().Kind)
}

func TestL3StreamPauseCancellation(t *testing.T) {
	for _, direction := range []string{"read", "write"} {
		t.Run(direction, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			var fault *streamFaultConn
			f := newL3FaultFixture(t, ctx, false, func(c net.Conn) net.Conn { fault = newStreamFaultConn(c); return fault })
			t.Cleanup(func() { _ = fault.Close() })
			c := f.dial(t, 22)
			if _, err := c.Write([]byte{1}); err != nil {
				t.Fatal(err)
			}
			var first [1]byte
			if _, err := io.ReadFull(c, first[:]); err != nil || first[0] != 1 {
				t.Fatalf("warmup: %x %v", first, err)
			}
			gate := &fault.read
			if direction == "write" {
				gate = &fault.write
			}
			pause := gate.pause()
			done := make(chan error, 1)
			go func() {
				_, err := c.Write([]byte("paused L3 flow"))
				if err == nil {
					_, err = io.ReadFull(c, make([]byte, 14))
				}
				done <- err
			}()
			streamFaultWait(t, pause.entered, direction+" L3 stream pause")
			f.cancel()
			_ = fault.Close()
			f.close(t)
			_ = c.Close()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("paused flow unexpectedly completed")
				}
			case <-ctx.Done():
				t.Fatal("paused application I/O did not join")
			}
			streamFaultWait(t, fault.drained, "paused underlying connection I/O")
			for _, g := range f.faults {
				if g.dropped.Load() != 0 || g.delayed.Load() != 0 {
					t.Fatal("stream pause counted as IP fault")
				}
			}
			t.Logf("stream %s pause entered and canceled; IP drops=0", direction)
		})
	}
}
