package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"path/filepath"
	"strings"

	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/ticket"
	"remote-enet/internal/trust"
)

// HubConfig pins the outer endpoint independently of the session peer.
type HubConfig struct {
	Addr string
	FP   identity.Fingerprint
}

func (h HubConfig) validate() error {
	if h.FP.IsZero() {
		return fmt.Errorf("app: nonzero hub fingerprint required")
	}
	addr := h.Addr
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("app: hub address: %w", err)
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.WithZone("").Unmap()
		if !ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() {
			return fmt.Errorf("app: hub address requires a unicast host")
		}
		// A local IPv6 scope is valid for an outbound dial; numeric share
		// addresses use the stricter, portable invitation address grammar.
		if strings.Contains(host, "%") {
			addr = net.JoinHostPort(ip.String(), port)
		}
	}
	_, err = invite.ParseAddress(addr)
	return err
}

func loadIssuer(roleDir string) (*ticket.Issuer, error) {
	return ticket.LoadOrCreateIssuer(filepath.Join(roleDir, "issuer.key"))
}

// IssuerEntry uses the same role directory as RunOperator and IssueInvite.
func IssuerEntry(ctx context.Context, stateDir, name string) (string, error) {
	if len(name) > 64 {
		return "", fmt.Errorf("app: issuer name exceeds 64 bytes")
	}
	for _, c := range name {
		if c < 32 || c > 126 {
			return "", fmt.Errorf("app: issuer name must be printable ASCII")
		}
	}
	store, err := trust.Open(ctx, stateDir)
	if err != nil {
		return "", err
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		return "", err
	}
	i, err := loadIssuer(stateDir)
	if err != nil {
		return "", err
	}
	entry, err := json.Marshal(hubauth.Issuer{Name: name, Operator: id.Fingerprint().String(), PublicKey: base64.RawURLEncoding.EncodeToString(i.PublicKey())})
	return string(entry), err
}
