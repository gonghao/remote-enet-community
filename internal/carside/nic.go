// Package carside discovers vehicle gateways and forwards opaque tunnel bytes.
package carside

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
)

var (
	// ErrNICUnavailable allows callers to wait for a selected address to reappear.
	ErrNICUnavailable = errors.New("carside: selected IPv4 interface unavailable")
	// ErrNICAmbiguous requires an explicit selection rather than an arbitrary address.
	ErrNICAmbiguous = errors.New("carside: ambiguous IPv4 interface selection")
)

// NIC identifies one IPv4 source address, not just an adapter with several addresses.
type NIC struct {
	Name    string
	Address netip.Addr
	Prefix  netip.Prefix
}

// Interfaces includes loopback for explicitly selected isolated tests.
func Interfaces() ([]NIC, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, fmt.Errorf("carside: enumerate interfaces: %w", err)
	}
	var result []NIC
	for _, nic := range interfaces {
		if nic.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, err := nic.Addrs()
		if err != nil {
			return nil, fmt.Errorf("carside: enumerate addresses on %s: %w", nic.Name, err)
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			n := NIC{Name: nic.Name, Address: prefix.Addr(), Prefix: prefix.Masked()}
			if validateNIC(n) == nil {
				result = append(result, n)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Address.Compare(result[j].Address) < 0
	})
	return result, nil
}

// SelectNIC chooses automatically only when one non-loopback link-local address exists.
func SelectNIC(name string, addr netip.Addr) (NIC, error) {
	nics, err := Interfaces()
	if err != nil {
		return NIC{}, err
	}
	return SelectNICFrom(nics, name, addr)
}

// SelectNICFrom applies the same selection to a fresh enumeration. A fixed
// address never falls back to another address on the named interface.
func SelectNICFrom(nics []NIC, name string, addr netip.Addr) (NIC, error) {
	if addr.IsValid() && (!addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() || addr == netip.MustParseAddr("255.255.255.255")) {
		return NIC{}, fmt.Errorf("carside: NIC address must be unicast IPv4")
	}
	explicit := name != "" || addr.IsValid()
	var matches []NIC
	for _, nic := range nics {
		if name != "" && nic.Name != name || addr.IsValid() && nic.Address != addr {
			continue
		}
		if !explicit && (!nic.Address.IsLinkLocalUnicast() || nic.Address.IsLoopback()) {
			continue
		}
		if err := validateNIC(nic); err != nil {
			return NIC{}, err
		}
		nic.Prefix = nic.Prefix.Masked()
		matches = append(matches, nic)
	}
	if len(matches) == 0 {
		return NIC{}, ErrNICUnavailable
	}
	if len(matches) > 1 {
		return NIC{}, fmt.Errorf("%w: matched %d addresses; specify --nic and --nic-addr", ErrNICAmbiguous, len(matches))
	}
	return matches[0], nil
}

func validateNIC(nic NIC) error {
	if !nic.Address.Is4() || !nic.Address.IsGlobalUnicast() && !nic.Address.IsLoopback() && !nic.Address.IsLinkLocalUnicast() {
		return fmt.Errorf("carside: invalid IPv4 source address %s", nic.Address)
	}
	if !nic.Prefix.IsValid() || !nic.Prefix.Addr().Is4() || !nic.Prefix.Contains(nic.Address) {
		return fmt.Errorf("carside: source address %s is outside IPv4 prefix %s", nic.Address, nic.Prefix)
	}
	if broadcast(nic.Address, nic.Prefix) || network(nic.Address, nic.Prefix) {
		return fmt.Errorf("carside: source address %s is not a subnet host", nic.Address)
	}
	return nil
}

func network(addr netip.Addr, prefix netip.Prefix) bool {
	return prefix.Bits() < 31 && addr == prefix.Masked().Addr()
}

func broadcast(addr netip.Addr, prefix netip.Prefix) bool {
	return addr == netip.AddrFrom4([4]byte{255, 255, 255, 255}) || prefix.Bits() < 31 && addr == directedBroadcast(prefix)
}

func directedBroadcast(prefix netip.Prefix) netip.Addr {
	ip := prefix.Masked().Addr().As4()
	value := binary.BigEndian.Uint32(ip[:])
	mask := ^uint32(0) >> uint(prefix.Bits())
	binary.BigEndian.PutUint32(ip[:], value|mask)
	return netip.AddrFrom4(ip)
}

func localIPv4Addresses() (map[netip.Addr]struct{}, error) {
	addresses, err := net.InterfaceAddrs()
	if err != nil {
		return nil, fmt.Errorf("carside: enumerate local addresses: %w", err)
	}
	result := make(map[netip.Addr]struct{})
	for _, address := range addresses {
		prefix, err := netip.ParsePrefix(address.String())
		if err == nil && prefix.Addr().Is4() {
			result[prefix.Addr()] = struct{}{}
		}
	}
	return result, nil
}
