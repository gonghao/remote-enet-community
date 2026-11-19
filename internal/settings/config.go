// Package settings persists GUI preferences separately from identities and trust.
package settings

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"remote-enet/internal/identity"
)

// Version changes when the persisted schema changes incompatibly.
const Version = 2

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
	Mode           string `json:"mode"`
	Hub            Hub    `json:"hub"`
	Listen         string `json:"listen"`
	Endpoint       string `json:"endpoint"`
	Bind           string `json:"bind"`
	DiagnosticPort int    `json:"diagnostic_port"`
	ControlPort    int    `json:"control_port"`
	DiscoveryPort  int    `json:"discovery_port"`
	Exclusive      bool   `json:"exclusive"`
}

// Hub stores the dial address and full outer TLS pin for Hub mode.
type Hub struct {
	Addr string `json:"addr"`
	FP   string `json:"fp"`
}

// Center is unconfigured when both ID and API are empty.
type Center struct {
	ID  string   `json:"id"`
	API []string `json:"api"`
}

// Config contains preferences for both roles, independent of the active role.
type Config struct {
	Version       int      `json:"version"`
	Role          string   `json:"role"`
	AllowLinkTest bool     `json:"allow_link_test"`
	Client        Client   `json:"client"`
	Operator      Operator `json:"operator"`
	Center        Center   `json:"center"`
}

// Equal compares configuration values, treating nil and empty API lists alike.
func (c Config) Equal(other Config) bool {
	return c.Version == other.Version && c.Role == other.Role &&
		c.AllowLinkTest == other.AllowLinkTest && c.Client == other.Client &&
		c.Operator == other.Operator && c.Center.ID == other.Center.ID &&
		slices.Equal(c.Center.API, other.Center.API)
}

// Default leaves addresses that require a user's choice unconfigured.
// Use Load to report a malformed build-time center.
func Default() Config {
	cfg := baseConfig()
	cfg.Center, _ = loadDefaultCenter()
	return cfg
}

func baseConfig() Config {
	return Config{
		Version:       Version,
		Role:          RoleClient,
		AllowLinkTest: true,
		Operator: Operator{
			Mode:           "direct",
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
	switch c.Operator.Mode {
	case "direct":
		if c.Operator.Hub.Addr != "" {
			return fmt.Errorf("settings: operator.hub.addr must be empty in direct mode")
		}
		if c.Operator.Hub.FP != "" {
			return fmt.Errorf("settings: operator.hub.fp must be empty in direct mode")
		}
		if c.Operator.Listen == "" {
			if c.Role == RoleOperator {
				return fmt.Errorf("settings: operator.listen is required for the operator role")
			}
		} else if err := address(c.Operator.Listen, true); err != nil {
			return fmt.Errorf("settings: operator.listen: %w", err)
		}
	case "hub":
		if err := address(c.Operator.Hub.Addr, false); err != nil {
			return fmt.Errorf("settings: operator.hub.addr: %w", err)
		}
		fp, err := identity.ParseFingerprint(c.Operator.Hub.FP)
		if err != nil || fp.IsZero() {
			return fmt.Errorf("settings: operator.hub.fp must be a nonzero device fingerprint")
		}
	default:
		return fmt.Errorf("settings: operator.mode must be direct or hub")
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
		return fmt.Errorf("settings: operator.diagnostic_port and operator.control_port must differ")
	}
	return c.Center.validate()
}

var centerID = regexp.MustCompile(`^[a-z0-9.-]{1,32}$`)

func (c Center) validate() error {
	if c.ID == "" && len(c.API) == 0 {
		return nil
	}
	if !centerID.MatchString(c.ID) {
		return fmt.Errorf("settings: center.id must match [a-z0-9.-]{1,32} when configured")
	}
	if len(c.API) < 1 || len(c.API) > 4 {
		return fmt.Errorf("settings: center.api must contain 1 to 4 URLs when configured")
	}
	for i, entry := range c.API {
		if len(entry) > 256 || !utf8.ValidString(entry) || strings.IndexFunc(entry, func(r rune) bool { return r == ' ' || unicode.IsControl(r) }) >= 0 {
			return fmt.Errorf("settings: center.api[%d] must be valid text of at most 256 bytes", i)
		}
		u, err := url.Parse(entry)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
			u.ForceQuery || u.RawQuery != "" || strings.Contains(entry, "#") {
			return fmt.Errorf("settings: center.api[%d] must be an absolute HTTPS URL without userinfo, query or fragment", i)
		}
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
