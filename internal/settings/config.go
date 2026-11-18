// Package settings persists GUI preferences separately from identities and trust.
package settings

import (
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"remote-enet/internal/identity"
)

// Version changes when the persisted schema changes incompatibly.
const Version = 1

const (
	// RoleClient selects the car-side role.
	RoleClient = "client"
	// RoleOperator selects the local diagnostic presentation role.
	RoleOperator = "operator"
)

// Client keeps only the paired fingerprint, never the invitation or its token.
// Empty NICName and NICAddr leave selection to the application.
type Client struct {
	NICName string `json:"nic_name"`
	NICAddr string `json:"nic_addr"`
	Peer    string `json:"peer"`
}

// Operator permits empty Endpoint and Bind until the controller starts listening.
// Diagnostic and control use distinct TCP ports; discovery uses UDP.
type Operator struct {
	Listen         string `json:"listen"`
	Endpoint       string `json:"endpoint"`
	Bind           string `json:"bind"`
	DiagnosticPort int    `json:"diagnostic_port"`
	ControlPort    int    `json:"control_port"`
	DiscoveryPort  int    `json:"discovery_port"`
	Exclusive      bool   `json:"exclusive"`
}

// Config contains preferences for both roles, independent of the active role.
type Config struct {
	Version       int      `json:"version"`
	Role          string   `json:"role"`
	AllowLinkTest bool     `json:"allow_link_test"`
	Client        Client   `json:"client"`
	Operator      Operator `json:"operator"`
}

// Default leaves addresses that require a user's choice unconfigured.
func Default() Config {
	return Config{
		Version:       Version,
		Role:          RoleClient,
		AllowLinkTest: true,
		Operator: Operator{
			Listen:         "0.0.0.0:47000",
			DiagnosticPort: 6801,
			ControlPort:    6811,
			DiscoveryPort:  6811,
			Exclusive:      runtime.GOOS == "windows",
		},
	}
}

// Validate checks inactive-role fields too. Endpoint and Bind may remain empty;
// the controller must require them before starting the operator.
func (c Config) Validate() error {
	if c.Version != Version {
		return fmt.Errorf("settings: unsupported version")
	}
	if c.Role != RoleClient && c.Role != RoleOperator {
		return fmt.Errorf("settings: role must be client or operator")
	}
	if !utf8.ValidString(c.Client.NICName) || strings.IndexFunc(c.Client.NICName, unicode.IsControl) >= 0 {
		return fmt.Errorf("settings: client.nic_name must not contain invalid text or control characters")
	}
	if c.Client.NICAddr != "" && !unicastIPv4(c.Client.NICAddr) {
		return fmt.Errorf("settings: client.nic_addr must be a literal unicast IPv4 address")
	}
	if c.Client.Peer != "" {
		fp, err := identity.ParseFingerprint(c.Client.Peer)
		if err != nil || fp.IsZero() || strings.ContainsAny(c.Client.Peer, "\r\n") {
			return fmt.Errorf("settings: client.peer must be a nonzero device fingerprint")
		}
	}
	if c.Operator.Listen == "" {
		if c.Role == RoleOperator {
			return fmt.Errorf("settings: operator.listen is required for the operator role")
		}
	} else if err := address(c.Operator.Listen, true); err != nil {
		return fmt.Errorf("settings: operator.listen: %w", err)
	}
	if c.Operator.Endpoint != "" {
		if err := address(c.Operator.Endpoint, false); err != nil {
			return fmt.Errorf("settings: operator.endpoint: %w", err)
		}
	}
	if c.Operator.Bind != "" && !unicastIPv4(c.Operator.Bind) {
		return fmt.Errorf("settings: operator.bind must be a literal unicast IPv4 address")
	}
	for _, p := range []struct {
		name  string
		value int
	}{
		{"diagnostic_port", c.Operator.DiagnosticPort},
		{"control_port", c.Operator.ControlPort},
		{"discovery_port", c.Operator.DiscoveryPort},
	} {
		if p.value < 1 || p.value > 65535 {
			return fmt.Errorf("settings: operator.%s must be in 1..65535", p.name)
		}
	}
	if c.Operator.DiagnosticPort == c.Operator.ControlPort {
		return fmt.Errorf("settings: operator diagnostic and control TCP ports must differ")
	}
	return nil
}

func unicastIPv4(value string) bool {
	addr, err := netip.ParseAddr(value)
	return err == nil && addr.Is4() && unicast(addr)
}

func unicast(addr netip.Addr) bool {
	// netip can classify scoped :: as global unicast unless its zone is removed.
	addr = addr.WithZone("").Unmap()
	return addr.IsGlobalUnicast() || addr.IsLinkLocalUnicast() || addr.IsLoopback()
}

func address(value string, listen bool) error {
	if !utf8.ValidString(value) || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("address must not contain invalid text or control characters")
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("address must be host:port with brackets around IPv6")
	}
	for _, c := range port {
		if c < '0' || c > '9' {
			return fmt.Errorf("port must be in 1..65535")
		}
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return fmt.Errorf("port must be in 1..65535")
	}
	if host == "" {
		if listen {
			return nil
		}
		return fmt.Errorf("host is required")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if strings.HasPrefix(value, "[") && !ip.Is6() {
			return fmt.Errorf("brackets require an IPv6 address")
		}
		unzoned := ip.WithZone("").Unmap()
		if unzoned.IsUnspecified() && ip.Zone() != "" {
			return fmt.Errorf("unspecified host must not include a zone")
		}
		if unicast(unzoned) || (listen && unzoned.IsUnspecified()) {
			return nil
		}
		return fmt.Errorf("host must be unicast or a listening wildcard")
	}
	if strings.HasPrefix(value, "[") || !hostname(host) {
		return fmt.Errorf("host must be a valid IP address or DNS hostname")
	}
	return nil
}

func hostname(host string) bool {
	host = strings.TrimSuffix(host, ".")
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	numeric := true
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for i := range len(label) {
			c := label[i]
			if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' {
				numeric = false
			} else if c < '0' || c > '9' {
				return false
			}
		}
	}
	return !numeric
}
