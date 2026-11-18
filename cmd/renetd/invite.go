package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"remote-enet/internal/app"
)

func runInvite(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("invite", stderr)
	stateDir := fs.String("state-dir", "", "operator state directory (required)")
	address := fs.String("endpoint", "", "externally reachable operator host:port (required)")
	ttl := fs.Duration("ttl", 15*time.Minute, "one-time invitation lifetime (at least 1s)")
	if err := parsed(fs, args, stateDir); err != nil {
		return err
	}
	if err := endpoint(*address); err != nil {
		return fmt.Errorf("--endpoint: %w", err)
	}
	if *ttl < time.Second {
		return fmt.Errorf("--ttl must be at least 1s")
	}
	code, err := app.IssueInvite(ctx, app.InviteConfig{StateDir: *stateDir, Endpoint: *address, TTL: *ttl})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, code); err != nil {
		return fmt.Errorf("write invitation: %w", err)
	}
	return nil
}
