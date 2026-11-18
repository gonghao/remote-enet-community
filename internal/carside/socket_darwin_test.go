package carside

import (
	"context"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

func TestNativeVehicleUDPInterfaceIsPinned(t *testing.T) {
	interfaces, err := net.Interfaces()
	if err != nil {
		t.Fatal(err)
	}
	nic := NIC{Name: "fixture", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}
	index, err := vehicleInterfaceIndex(nic, interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
	if err != nil {
		t.Fatal(err)
	}
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error { return bindVehicleInterface(t.Context(), nic, raw) }}
	packet, err := lc.ListenPacket(t.Context(), "udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer packet.Close()
	raw, err := packet.(*net.UDPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var actual int
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		actual, optionErr = syscall.GetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF)
	}); err != nil {
		t.Fatal(err)
	}
	if optionErr != nil || actual != index {
		t.Fatalf("socket not pinned: %d/%d %v", actual, index, optionErr)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := bindVehicleInterface(ctx, nic, raw); err != context.Canceled {
		t.Fatalf("canceled control: %v", err)
	}
}
