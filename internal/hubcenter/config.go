// Package hubcenter implements center-mode Hub configuration and admission.
package hubcenter

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"remote-enet/internal/hubauth"
	"remote-enet/internal/settings"
)

// Contract durations are fixed for all center nodes.
const (
	ClockSkew        = 30 * time.Second
	TicketMaxTTL     = 300 * time.Second
	HeartbeatTimeout = 10 * time.Second
)

// Config contains decoded identifiers and keys ready for activation and reload.
type Config struct {
	Version                   int
	Mode, IdentityDir, Listen string
	Center                    settings.Center
	NodeID                    [16]byte
	TicketKeys                []ed25519.PublicKey
	SetupLimits               hubauth.SetupLimits
}

// Load validates configuration without opening the identity directory.
func Load(path string) (Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	var file struct {
		Version     int                 `json:"version"`
		Mode        string              `json:"mode"`
		IdentityDir string              `json:"identity_dir"`
		Listen      string              `json:"listen"`
		Center      settings.Center     `json:"center"`
		NodeID      string              `json:"node_id"`
		TicketKeys  []string            `json:"ticket_keys"`
		SetupLimits hubauth.SetupLimits `json:"setup_limits"`
	}
	file.SetupLimits = hubauth.SetupLimits{Concurrent: 64, PerSecond: 32, Burst: 64, TimeoutS: 10}
	if err := hubauth.DecodeStrict(f, 64<<10, &file); err != nil {
		return Config{}, err
	}
	node, err := decodeBytes(file.NodeID, 16)
	if err != nil {
		return Config{}, fmt.Errorf("node_id: %w", err)
	}
	c := Config{Version: file.Version, Mode: file.Mode,
		IdentityDir: file.IdentityDir, Listen: file.Listen,
		Center: file.Center, NodeID: [16]byte(node), SetupLimits: file.SetupLimits,
	}
	for i, s := range file.TicketKeys {
		key, err := decodeBytes(s, ed25519.PublicKeySize)
		if err != nil {
			return Config{}, fmt.Errorf("ticket_keys[%d]: %w", i, err)
		}
		c.TicketKeys = append(c.TicketKeys, key)
	}
	if err := c.validate(); err != nil {
		return Config{}, err
	}
	if _, err := newKeySet(c.TicketKeys); err != nil {
		return Config{}, err
	}
	return c, nil
}

func decodeBytes(s string, n int) ([]byte, error) {
	if len(s) != base64.RawURLEncoding.EncodedLen(n) || strings.ContainsAny(s, "\r\n") {
		return nil, fmt.Errorf("expected %d bytes in unpadded base64url", n)
	}
	return base64.RawURLEncoding.Strict().DecodeString(s)
}

var centerID = regexp.MustCompile("^[a-z0-9.-]{1,32}$")

func (c Config) validate() error {
	if c.Version != 1 {
		return errors.New("version: expected 1")
	}
	if c.Mode != "center" {
		return errors.New("mode: expected center")
	}
	if strings.TrimSpace(c.IdentityDir) == "" || strings.ContainsRune(c.IdentityDir, 0) {
		return errors.New("identity_dir: required")
	}
	if !validListen(c.Listen) {
		return errors.New("listen: expected host:port")
	}
	if !centerID.MatchString(c.Center.ID) {
		return errors.New("center.id: invalid identifier")
	}
	if len(c.Center.API) < 1 || len(c.Center.API) > 4 {
		return errors.New("center.api: expected 1..4 entries")
	}
	for i, entry := range c.Center.API {
		u, err := url.Parse(entry)
		if err != nil || len(entry) > 256 ||
			!utf8.ValidString(entry) || strings.ContainsAny(entry, " #") || strings.IndexFunc(entry, unicode.IsControl) >= 0 ||
			u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" {
			return fmt.Errorf("center.api[%d]: expected absolute HTTPS URL without userinfo, query or fragment", i)
		}
	}
	l := c.SetupLimits
	if l.Concurrent < 1 || l.PerSecond < 1 || l.Burst < 1 ||
		l.TimeoutS < 1 || l.TimeoutS > 30 {
		return errors.New("setup_limits: expected positive limits and timeout_s in 1..30")
	}
	return nil
}

// ReloadCompatible compares effective startup fields, including API failover order.
func (c Config) ReloadCompatible(old Config) error {
	if c.Version != old.Version || c.Mode != old.Mode || c.IdentityDir != old.IdentityDir || c.Listen != old.Listen ||
		c.Center.ID != old.Center.ID || !slices.Equal(c.Center.API, old.Center.API) ||
		c.NodeID != old.NodeID || c.SetupLimits != old.SetupLimits {
		return errors.New("configuration change requires restart")
	}
	return nil
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
