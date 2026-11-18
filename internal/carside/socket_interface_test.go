package carside

import (
	"errors"
	"net"
	"net/netip"
	"testing"
)

func TestVehicleSocketSelectionCannotFollowVPNSource(t *testing.T) {
	nic := NIC{Name: "en9", Address: netip.MustParseAddr("169.254.177.23"), Prefix: netip.MustParsePrefix("169.254.0.0/16")}
	interfaces := []net.Interface{{Index: 1, Name: "utun8", Flags: net.FlagUp}, {Index: 2, Name: "en0", Flags: net.FlagUp}, {Index: 33, Name: "en9", Flags: net.FlagUp}}
	addresses := func(iface net.Interface) ([]net.Addr, error) {
		if iface.Name == "en9" {
			return []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.177.23"), Mask: net.CIDRMask(16, 32)}}, nil
		}
		return []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.177.23"), Mask: net.CIDRMask(16, 32)}}, nil
	}
	index, err := vehicleInterfaceIndex(nic, interfaces, addresses)
	if err != nil || index != 33 {
		t.Fatalf("wrong selected interface: %d %v", index, err)
	}
	if _, err := vehicleInterfaceIndex(nic, interfaces[:2], addresses); err == nil {
		t.Fatal("missing saved adapter fell back to VPN/Wi-Fi")
	}
	interfaces[2].Flags = 0
	if _, err := vehicleInterfaceIndex(nic, interfaces, addresses); err == nil {
		t.Fatal("down saved adapter accepted")
	}
	interfaces[2].Flags = net.FlagUp
	if _, err := vehicleInterfaceIndex(nic, interfaces, func(net.Interface) ([]net.Addr, error) { return nil, errors.New("enumeration failed") }); err == nil {
		t.Fatal("address error ignored")
	}
}
