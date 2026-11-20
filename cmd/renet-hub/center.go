package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

var version = "0.0.0-dev"
var errCenterServe = errors.New("center serve")

func exitCode(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, hubcenter.ErrSuperseded):
		return 3
	case errors.Is(err, errCenterServe):
		return 2
	default:
		return 1
	}
}

// The selected loader validates the entire document, including the mode.
func configMode(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("open configuration: %w", err)
	}
	defer f.Close()
	var header struct {
		Mode string `json:"mode"`
	}
	err = json.NewDecoder(io.LimitReader(f, 64<<10)).Decode(&header)
	return header.Mode, err
}

func runCenter(ctx context.Context, command, path string, online bool, out io.Writer, logger *slog.Logger) error {
	if command == "ticket inspect" {
		return errors.New("ticket inspect is unsupported in center mode")
	}
	cfg, err := hubcenter.Load(path)
	if err != nil {
		return err
	}
	local, err := identity.LoadExisting(cfg.IdentityDir)
	if err != nil {
		return fmt.Errorf("identity_dir: %w", err)
	}
	if command == "check" {
		return checkCenter(ctx, cfg, local.Fingerprint(), online, out)
	}
	return serveCenter(ctx, path, cfg, local, logger)
}

func checkCenter(ctx context.Context, cfg hubcenter.Config, fp identity.Fingerprint, online bool, out io.Writer) error {
	if _, err := fmt.Fprintf(out, "center_id=%s node_id=%s hub=%s\n", cfg.Center.ID, center.NodeID(cfg.NodeID), fp); err != nil {
		return err
	}
	for _, k := range cfg.TicketKeys {
		if _, err := fmt.Fprintf(out, "kid=%s\n", center.KeyID(ticket.KidOf(k))); err != nil {
			return err
		}
	}
	if !online {
		for _, entry := range cfg.Center.API {
			if _, err := fmt.Fprintf(out, "entry=%s\n", entry); err != nil {
				return err
			}
		}
		return nil
	}
	var failed bool
	for _, r := range center.Check(ctx, cfg.Center, "hub", version) {
		if _, err := fmt.Fprintf(out, "entry=%s status=%s\n", r.Entry, r.Status); err != nil {
			return err
		}
		failed = failed || r.Status != center.Verified
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if failed {
		return errors.New("center entry verification failed")
	}
	return nil
}

func serveCenter(ctx context.Context, path string, cfg hubcenter.Config, local *identity.Local, logger *slog.Logger) error {
	p, err := hubcenter.NewPolicy(cfg, local.Fingerprint())
	if err != nil {
		return err
	}
	api, err := center.NewClient(center.ClientOptions{
		Center: cfg.Center, Role: "hub", Node: cfg.NodeID, NodeKey: local.Key,
		AppVersion: version, Logger: logger,
	})
	if err != nil {
		return err
	}
	limits := cfg.SetupLimits
	s := hub.NewServer(hub.Options{
		Local: local, Policy: p, Logger: logger,
		SetupLimits: hub.SetupLimits{
			Concurrent: limits.Concurrent, PerSecond: limits.PerSecond, Burst: limits.Burst,
			Timeout: time.Duration(limits.TimeoutS) * time.Second,
		},
	})
	node := hubcenter.NewNode(hubcenter.NodeOptions{
		Config: cfg, Local: local, Policy: p, API: api, Server: s,
		Version: version, Logger: logger,
	})
	signals, stop := reloadSignals()
	defer stop()
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	served, finished := make(chan error, 1), make(chan error, 1)
	go func() { served <- s.Serve(child, ln) }()
	go func() { finished <- node.Run(child) }()
	for {
		select {
		case err = <-finished:
			cancel()
			<-served
			return err
		case err = <-served:
			cancel()
			nodeErr := <-finished
			if errors.Is(nodeErr, hubcenter.ErrSuperseded) {
				return nodeErr
			}
			return err
		case <-signals:
			if err := reloadCenter(path, node); err != nil {
				logger.Error("Hub reload rejected", "error", err)
			} else {
				logger.Info("Hub ticket keys reloaded")
			}
		}
	}
}

func reloadCenter(path string, node *hubcenter.Node) error {
	cfg, err := hubcenter.Load(path)
	if err != nil {
		return errors.New("invalid or unreadable configuration; run renet-hub check for details")
	}
	return node.Reload(cfg)
}
