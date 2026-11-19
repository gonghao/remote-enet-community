package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"time"

	"remote-enet/internal/app"
)

func runInvite(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("invite", stderr)
	stateDir := fs.String("state-dir", "", "operator state directory (required)")
	hubFlags := addHubFlags(fs)
	address := fs.String("endpoint", "", "externally reachable operator host:port for a direct invitation")
	ttl := fs.Duration("ttl", 15*time.Minute, "one-time invitation lifetime (at least 1s)")
	if err := parsed(fs, args, stateDir); err != nil {
		return err
	}
	hub, err := hubFlags.config(fs, "endpoint")
	if err != nil {
		return err
	}
	if hub == nil {
		if err := endpoint(*address); err != nil {
			return fmt.Errorf("--endpoint: %w", err)
		}
	}
	if *ttl < time.Second {
		return fmt.Errorf("--ttl must be at least 1s")
	}
	code, err := app.IssueInvite(ctx, app.InviteConfig{StateDir: *stateDir, Endpoint: *address, TTL: *ttl, Hub: hub, Logger: slog.New(slog.NewTextHandler(stderr, nil))})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(stdout, code); err != nil {
		return fmt.Errorf("write invitation: %w", err)
	}
	return nil
}

func runIssuer(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flags("issuer", stderr)
	dir := fs.String("state-dir", "", "operator state directory (required)")
	name := fs.String("name", "", "label for the hub configuration")
	if err := parsed(fs, args, dir); err != nil {
		return err
	}
	entry, err := app.IssuerEntry(ctx, *dir, *name)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(stdout, entry)
	return err
}
