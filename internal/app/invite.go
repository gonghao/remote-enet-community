package app

import (
	"context"
	"fmt"
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
}

// IssueInvite returns a secret code. Only its hash is persisted by the store.
func IssueInvite(ctx context.Context, cfg InviteConfig) (string, error) {
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
	expires := time.Now().Add(cfg.TTL).Truncate(time.Second)
	token, err := store.Issue(ctx, expires)
	if err != nil {
		return "", err
	}
	code, err := (invite.Invite{
		Version:   1,
		Peer:      transport.PeerInfo{ID: id.Fingerprint(), Endpoints: []transport.Endpoint{{Kind: "hub", Addr: cfg.Endpoint, Hub: id.Fingerprint()}}},
		Token:     token,
		ExpiresAt: expires,
	}).Encode()
	if err != nil {
		return "", err
	}
	return code, nil
}
