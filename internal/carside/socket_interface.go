package carside

import (
	"fmt"
	"net"
	"net/netip"
)

func vehicleInterfaceIndex(nic NIC, interfaces []net.Interface, addresses func(net.Interface) ([]net.Addr, error)) (int, error) {
	var exact, loopback []int
	for _, iface := range interfaces {
		if iface.Index <= 0 || iface.Flags&net.FlagUp == 0 {
			continue
		}
		// Loopback test fixtures intentionally use synthetic NIC names. Real
		// adapters must match both the saved name and its exact source address.
		if !nic.Address.IsLoopback() && iface.Name != nic.Name {
			continue
		}
		values, err := addresses(iface)
		if err != nil {
			return 0, fmt.Errorf("carside: inspect selected interface: %w", err)
		}
		for _, value := range values {
			prefix, err := netip.ParsePrefix(value.String())
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			if prefix.Addr() == nic.Address {
				exact = append(exact, iface.Index)
			} else if nic.Address.IsLoopback() && iface.Flags&net.FlagLoopback != 0 && prefix.Contains(nic.Address) {
				loopback = append(loopback, iface.Index)
			}
		}
	}
	if len(exact) == 0 {
		exact = loopback
	}
	if len(exact) != 1 {
		return 0, fmt.Errorf("carside: selected source matches %d active interfaces", len(exact))
	}
	return exact[0], nil
}
