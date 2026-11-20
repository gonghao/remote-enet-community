package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
)

func serve(ctx context.Context, path string, cfg hubauth.Config, local *identity.Local, out io.Writer, logger *slog.Logger) error {
	fp := local.Fingerprint()
	policy, err := hubauth.NewPolicy(cfg, fp)
	if err != nil {
		return err
	}
	limits := cfg.SetupLimits
	server := hub.NewServer(hub.Options{
		Local: local, Policy: policy, Logger: logger,
		SetupLimits: hub.SetupLimits{
			Concurrent: limits.Concurrent, PerSecond: limits.PerSecond, Burst: limits.Burst,
			Timeout: time.Duration(limits.TimeoutS) * time.Second,
		},
	})
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	signals, stop := reloadSignals()
	defer stop()
	return serveListener(ctx, path, cfg, fp, server, ln, signals, out, logger)
}

func serveListener(ctx context.Context, path string, cfg hubauth.Config, fp identity.Fingerprint, server *hub.Server, ln net.Listener, signals <-chan os.Signal, out io.Writer, logger *slog.Logger) error {
	if _, err := fmt.Fprintf(out, "hub=%s listen=%s issuers=%d pairing=%s\n", fp, ln.Addr(), len(cfg.Issuers), pairing(cfg)); err != nil {
		ln.Close()
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	for {
		select {
		case err := <-done:
			if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
				return nil
			}
			return err
		case <-signals:
			if err := reload(path, &cfg, fp, server, logger); err != nil {
				logger.Error("Hub reload rejected", "error", err)
			}
		}
	}
}

func reload(path string, current *hubauth.Config, fp identity.Fingerprint, server *hub.Server, logger *slog.Logger) error {
	next, err := hubauth.Load(path)
	if err != nil {
		// Decoder errors can quote arbitrary JSON keys. Keep them out of service logs.
		return errors.New("invalid or unreadable configuration; run renet-hub check for details")
	}
	if err := next.ReloadCompatible(*current); err != nil {
		return err
	}
	policy, err := hubauth.NewPolicy(next, fp)
	if err != nil {
		return err
	}
	waits, pairs := server.SetPolicy(policy)
	*current = next
	logger.Info("Hub policy reloaded", "closed_waits", waits, "closed_pairs", pairs)
	return nil
}
