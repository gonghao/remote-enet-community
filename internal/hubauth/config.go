// Package hubauth implements standalone Hub admission and revocation.
package hubauth

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

// Config contains the effective settings after file defaults are applied.
type Config struct {
	Version           int         `json:"version"`
	Mode              string      `json:"mode"`
	IdentityDir       string      `json:"identity_dir"`
	Listen            string      `json:"listen"`
	Issuers           []Issuer    `json:"issuers"`
	PairingOperator   string      `json:"pairing_operator"`
	TicketMaxTTLS     int64       `json:"ticket_max_ttl_s"`
	BootstrapMaxTTLS  int64       `json:"bootstrap_max_ttl_s"`
	PairingWindowMaxS int64       `json:"pairing_window_max_s"`
	ClockSkewS        int64       `json:"clock_skew_s"`
	SetupLimits       SetupLimits `json:"setup_limits"`
	Deny              Deny        `json:"deny"`
}

// Issuer binds one signing key to one Operator.
type Issuer struct {
	Name      string `json:"name"`
	Operator  string `json:"operator"`
	PublicKey string `json:"public_key"`
}

// SetupLimits applies for the lifetime of a listener.
type SetupLimits struct {
	Concurrent int `json:"concurrent"`
	PerSecond  int `json:"per_second"`
	Burst      int `json:"burst"`
	TimeoutS   int `json:"timeout_s"`
}

// Deny contains independently revocable device fingerprints and ticket IDs.
type Deny struct {
	Devices []string `json:"devices"`
	JTIs    []string `json:"jtis"`
}

func defaults() Config {
	return Config{TicketMaxTTLS: 2678400, BootstrapMaxTTLS: 86400, PairingWindowMaxS: 900, ClockSkewS: 30, SetupLimits: SetupLimits{64, 32, 64, 10}}
}

var hostnameLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func validListen(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return !strings.HasPrefix(addr, "[") || ip.Is6()
	}
	if strings.HasPrefix(addr, "[") {
		return false
	}
	if host == "" {
		return true
	}
	host = strings.TrimSuffix(host, ".")
	if len(host) > 253 {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func decodeBytes(s string, n int) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n") || len(s) != base64.RawURLEncoding.EncodedLen(n) {
		return nil, fmt.Errorf("expected %d bytes in unpadded base64url", n)
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

func fingerprint(s string) (identity.Fingerprint, error) {
	fp, err := identity.ParseFingerprint(s)
	if err != nil || fp.IsZero() || !strings.EqualFold(strings.ReplaceAll(s, "-", ""), strings.ReplaceAll(fp.String(), "-", "")) {
		return identity.Fingerprint{}, errors.New("expected full nonzero fingerprint")
	}
	return fp, nil
}

// identity.Load repairs a missing certificate. Offline checks must be read-only.
func checkIdentity(dir string) error {
	data, err := os.ReadFile(filepath.Join(dir, "device.key"))
	if err != nil {
		return err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return errors.New("expected PKCS8 private key")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return err
	}
	if _, ok := key.(ed25519.PrivateKey); !ok {
		return errors.New("expected Ed25519 private key")
	}
	return nil
}

// Validate checks the configuration, including readability of the existing identity.
func (c Config) Validate() error {
	_, err := c.compile()
	return err
}

func (c Config) compile() (*Policy, error) {
	if c.Version != 1 {
		return nil, errors.New("version: expected 1")
	}
	if c.Mode != "selfhost" {
		return nil, errors.New("mode: expected selfhost")
	}
	if c.IdentityDir == "" {
		return nil, errors.New("identity_dir: required")
	}
	if err := checkIdentity(c.IdentityDir); err != nil {
		return nil, fmt.Errorf("identity_dir: %w", err)
	}
	if !validListen(c.Listen) {
		return nil, errors.New("listen: expected host:port")
	}
	if len(c.Issuers) < 1 || len(c.Issuers) > 64 {
		return nil, errors.New("issuers: expected 1..64 entries")
	}
	for _, r := range []struct {
		field           string
		value, min, max int64
	}{
		{"ticket_max_ttl_s", c.TicketMaxTTLS, 2592000, 31536000},
		{"bootstrap_max_ttl_s", c.BootstrapMaxTTLS, 300, 86400},
		{"pairing_window_max_s", c.PairingWindowMaxS, 300, 3600},
		{"clock_skew_s", c.ClockSkewS, 5, 300},
		{"setup_limits.timeout_s", int64(c.SetupLimits.TimeoutS), 1, 30},
	} {
		if r.value < r.min || r.value > r.max {
			return nil, fmt.Errorf("%s: expected %d..%d", r.field, r.min, r.max)
		}
	}
	for _, r := range []struct {
		field string
		value int
	}{{"concurrent", c.SetupLimits.Concurrent},
		{"per_second", c.SetupLimits.PerSecond},
		{"burst", c.SetupLimits.Burst}} {
		if r.value < 1 {
			return nil, fmt.Errorf("setup_limits.%s: expected positive integer", r.field)
		}
	}
	p := &Policy{
		issuers:      make(map[ticket.Kid]binding),
		devices:      make(map[identity.Fingerprint]bool),
		jtis:         make(map[[16]byte]bool),
		skew:         c.ClockSkewS,
		maxTTL:       c.TicketMaxTTLS,
		bootstrapTTL: c.BootstrapMaxTTLS,
		window:       c.PairingWindowMaxS,
	}
	operators := make(map[identity.Fingerprint]bool)
	for i, v := range c.Issuers {
		if len(v.Name) > 64 || strings.IndexFunc(v.Name, func(r rune) bool { return r < 32 || r > 126 }) >= 0 {
			return nil, fmt.Errorf("issuers[%d].name: expected at most 64 printable ASCII bytes", i)
		}
		op, err := fingerprint(v.Operator)
		if err != nil {
			return nil, fmt.Errorf("issuers[%d].operator: %w", i, err)
		}
		pub, err := decodeBytes(v.PublicKey, ed25519.PublicKeySize)
		if err != nil {
			return nil, fmt.Errorf("issuers[%d].public_key: %w", i, err)
		}
		kid := ticket.KidOf(pub)
		if _, ok := p.issuers[kid]; ok {
			return nil, fmt.Errorf("issuers[%d].public_key: duplicate kid", i)
		}
		p.issuers[kid] = binding{pub, op}
		operators[op] = true
	}
	if c.PairingOperator != "" {
		op, err := fingerprint(c.PairingOperator)
		if err != nil || !operators[op] {
			return nil, errors.New("pairing_operator: expected configured operator")
		}
		p.operator = op
	}
	if len(c.Deny.Devices) > 10000 {
		return nil, errors.New("deny.devices: too many entries")
	}
	if len(c.Deny.JTIs) > 10000 {
		return nil, errors.New("deny.jtis: too many entries")
	}
	for i, s := range c.Deny.Devices {
		fp, err := fingerprint(s)
		if err != nil {
			return nil, fmt.Errorf("deny.devices[%d]: %w", i, err)
		}
		p.devices[fp] = true
	}
	for i, s := range c.Deny.JTIs {
		b, err := decodeBytes(s, 16)
		if err != nil {
			return nil, fmt.Errorf("deny.jtis[%d]: %w", i, err)
		}
		p.jtis[[16]byte(b)] = true
	}
	return p, nil
}

// ReloadCompatible compares settings captured at server startup.
func (c Config) ReloadCompatible(old Config) error {
	for _, v := range []struct {
		field   string
		changed bool
	}{{"identity_dir", c.IdentityDir != old.IdentityDir},
		{"listen", c.Listen != old.Listen},
		{"mode", c.Mode != old.Mode},
		{"setup_limits", c.SetupLimits != old.SetupLimits}} {
		if v.changed {
			return fmt.Errorf("%s: change requires restart", v.field)
		}
	}
	return nil
}
