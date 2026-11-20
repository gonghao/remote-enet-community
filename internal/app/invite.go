package app

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"time"

	"remote-enet/internal/invite"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

// InviteConfig shares its state directory with the receiving operator.
type InviteConfig struct {
	StateDir string
	Endpoint string
	TTL      time.Duration
	Hub      *HubConfig
	Logger   *slog.Logger
}

// IssueInvite returns a secret code. Only its hash is persisted by the store.
func IssueInvite(ctx context.Context, cfg InviteConfig) (string, error) {
	if cfg.Hub != nil {
		if err := cfg.Hub.validate(); err != nil {
			return "", err
		}
		if cfg.TTL > 24*time.Hour {
			return "", fmt.Errorf("app: hub invitation lifetime exceeds 24h")
		}
		if cfg.Endpoint == "" {
			cfg.Endpoint = cfg.Hub.Addr
		}
	}
	if err := endpoint(cfg.Endpoint); err != nil {
		return "", err
	}
	if cfg.TTL < time.Second {
		return "", fmt.Errorf("app: invitation lifetime must be at least 1s")
	}
	store, err := trust.Open(ctx, cfg.StateDir)
	if err != nil {
		return "", err
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		return "", err
	}
	now := time.Now()
	expires := now.Add(cfg.TTL).Truncate(time.Second)
	ep := transport.Endpoint{Kind: "hub", Addr: cfg.Endpoint, Hub: id.Fingerprint()}
	version := uint8(1)
	var jti [16]byte
	if cfg.Hub != nil {
		if cfg.Hub.FP == id.Fingerprint() {
			return "", fmt.Errorf("app: standalone hub requires a separate identity")
		}
		issuer, err := loadIssuer(cfg.StateDir)
		if err != nil {
			return "", err
		}
		raw, handle, err := issuer.Bootstrap(now, expires, id.Fingerprint(), cfg.Hub.FP)
		if err != nil {
			return "", err
		}
		version, ep.Hub, ep.Ticket, jti = 2, cfg.Hub.FP, string(raw), handle
	}
	token, err := store.Issue(ctx, expires)
	if err != nil {
		return "", err
	}
	code, err := (invite.Invite{
		Version:   version,
		Peer:      transport.PeerInfo{ID: id.Fingerprint(), Endpoints: []transport.Endpoint{ep}},
		Token:     token,
		ExpiresAt: expires,
	}).Encode()
	if err != nil {
		return "", err
	}
	if cfg.Hub != nil {
		logger := cfg.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.Info("issued hub invitation", "jti", base64.RawURLEncoding.EncodeToString(jti[:]))
	}
	return code, nil
}
