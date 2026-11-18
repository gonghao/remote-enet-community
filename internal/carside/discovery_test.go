package carside

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/testutil/fakezgw"
)

func identificationFrame(t *testing.T) []byte {
	t.Helper()
	id := hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, VIN: "WBAXXXXXXX0000001"}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: append(id.MarshalBody(), []byte("EXTENSION123")...)}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func udpListener(t *testing.T, ip string) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(netip.MustParseAddr(ip), 0)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if err := c.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDiscoverPreservesRawAndUsesSelectedSource(t *testing.T) {
	c := udpListener(t, "127.0.0.2")
	outsider := udpListener(t, "127.0.1.2")
	raw := identificationFrame(t)
	serverDone := make(chan error, 1)
	go func() {
		buffer := make([]byte, 100)
		n, peer, err := c.ReadFromUDPAddrPort(buffer)
		if err != nil {
			serverDone <- err
			return
		}
		if peer.Addr() != testNIC().Address || peer.Port() == 0 || !bytes.Equal(buffer[:n], hsfz.IdentRequest) {
			serverDone <- errors.New("discovery source or request changed")
			return
		}
		wrongControl := bytes.Clone(raw)
		wrongControl[5] = 1
		truncatedBody := bytes.Clone(raw)
		truncatedBody[3]++
		for _, packet := range [][]byte{{0, 0}, raw[:6], wrongControl, truncatedBody, append(bytes.Clone(raw), 0), raw, raw} {
			if _, err := c.WriteToUDPAddrPort(packet, peer); err != nil {
				serverDone <- err
				return
			}
		}
		_, err = outsider.WriteToUDPAddrPort(raw, peer)
		serverDone <- err
	}()
	nic := testNIC()
	nic.Prefix = netip.MustParsePrefix("127.0.0.0/24")
	gateways, err := Discover(t.Context(), DiscoverOptions{NIC: nic, Timeout: 80 * time.Millisecond, Destinations: []netip.AddrPort{c.LocalAddr().(*net.UDPAddr).AddrPort()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	if len(gateways) != 1 {
		t.Fatalf("got %d gateways", len(gateways))
	}
	g := gateways[0]
	if g.Address.String() != "127.0.0.2" || g.Identification.VIN != "WBAXXXXXXX0000001" || !bytes.Equal(g.IdentRaw, raw) || !bytes.Equal(g.Identification.Raw, raw[6:]) {
		t.Fatalf("unexpected gateway: %+v", g)
	}
	state := g.VehicleState(nic, 7)
	if state.Seq != 7 || state.NIC != nic.Name || state.NICAddr != nic.Address.String() || state.GatewayAddr != g.Address.String() || state.VIN != g.Identification.VIN || state.MAC != "aa:bb:cc:dd:ee:ff" || state.DiagAddr != 0x10 || !bytes.Equal(state.IdentRaw, raw) {
		t.Fatalf("unexpected state: %+v", state)
	}
	state.IdentRaw[0] = 0xff
	if bytes.Equal(state.IdentRaw, g.IdentRaw) {
		t.Fatal("VehicleState shares mutable packet data")
	}
}

func TestDiscoverFakeGatewayAndCancellation(t *testing.T) {
	id := hsfz.Identification{VIN: "WBAXXXXXXX0000001", DiagAddr: 0x10}
	s, err := fakezgw.Start(t.Context(), netip.MustParseAddr("127.0.0.2"), fakezgw.Profile{Ident: id}, fakezgw.WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	_, _, disc := s.Addr()
	o := DiscoverOptions{NIC: testNIC(), Timeout: 20 * time.Millisecond, Destinations: []netip.AddrPort{disc.AddrPort()}}
	gateways, err := Discover(t.Context(), o)
	if err != nil || len(gateways) != 1 || gateways[0].Identification.VIN != id.VIN {
		t.Fatalf("fake gateway discovery: %+v, %v", gateways, err)
	}
	idle := udpListener(t, "127.0.0.2")
	o.Destinations = []netip.AddrPort{idle.LocalAddr().(*net.UDPAddr).AddrPort()}
	o.Timeout = time.Second
	for _, deadline := range []bool{false, true} {
		ctx, cancel := context.WithCancel(t.Context())
		want := context.Canceled
		if deadline {
			cancel()
			ctx, cancel = context.WithTimeout(t.Context(), 10*time.Millisecond)
			want = context.DeadlineExceeded
		} else {
			time.AfterFunc(10*time.Millisecond, cancel)
		}
		start := time.Now()
		_, err := Discover(ctx, o)
		cancel()
		if !errors.Is(err, want) || time.Since(start) > time.Second/2 {
			t.Fatalf("discovery cancellation: %v", err)
		}
	}
}

func TestDiscoverRejectsInvalidConfiguration(t *testing.T) {
	for _, o := range []DiscoverOptions{
		{},
		{NIC: testNIC(), Timeout: -time.Second},
		{NIC: testNIC(), Destinations: []netip.AddrPort{netip.MustParseAddrPort("[::1]:6811")}},
		{NIC: testNIC(), Destinations: []netip.AddrPort{netip.MustParseAddrPort("127.0.0.2:0")}},
	} {
		if _, err := Discover(t.Context(), o); err == nil {
			t.Fatalf("invalid options accepted: %+v", o)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Discover(ctx, DiscoverOptions{NIC: testNIC()}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
