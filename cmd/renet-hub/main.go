// Command renet-hub runs a standalone self-hosted Hub.
package main

import (
	"context"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

const usage = "Usage: renet-hub init|fingerprint|check|serve|ticket inspect [options]"

func run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	command := args[0]
	args = args[1:]
	if command == "help" || command == "--help" || command == "-h" {
		_, err := fmt.Fprintln(stdout, usage)
		return err
	}
	if command == "ticket" {
		if len(args) == 0 || args[0] != "inspect" {
			return errors.New("usage: renet-hub ticket inspect --config FILE --ticket-file FILE [--device FP]")
		}
		command = "ticket inspect"
		args = args[1:]
	} else if command == "ticket inspect" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var dir, config, file, device string
	var debug bool
	switch command {
	case "init", "fingerprint":
		fs.StringVar(&dir, "identity-dir", "", "Hub identity directory")
	case "serve", "check", "ticket inspect":
		fs.StringVar(&config, "config", "", "Hub configuration file")
		if command == "serve" {
			fs.BoolVar(&debug, "debug", false, "log admission rejection reasons")
		}
		if command == "ticket inspect" {
			fs.StringVar(&file, "ticket-file", "", "unpadded base64url ticket file")
			fs.StringVar(&device, "device", "", "outer TLS device fingerprint")
		}
	default:
		return errors.New(usage)
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if fs.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "init" || command == "fingerprint" {
		if dir == "" {
			return errors.New("--identity-dir is required")
		}
		load := identity.LoadExisting
		if command == "init" {
			load = identity.Create
		}
		local, err := load(dir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, local.Fingerprint())
		return err
	}
	if config == "" {
		return errors.New("--config is required")
	}
	cfg, err := hubauth.Load(config)
	if err != nil {
		return err
	}
	local, err := identity.LoadExisting(cfg.IdentityDir)
	if err != nil {
		return fmt.Errorf("identity_dir: %w", err)
	}
	switch command {
	case "check":
		for _, issuer := range cfg.Issuers {
			op, _ := identity.ParseFingerprint(issuer.Operator)
			pub, _ := base64.RawURLEncoding.DecodeString(issuer.PublicKey)
			fmt.Fprintf(stdout, "name=%q operator=%s kid=%x\n", issuer.Name, op.Short(), ticket.KidOf(pub))
		}
		_, err = fmt.Fprintf(stdout, "pairing=%s\n", pairing(cfg))
		return err
	case "ticket inspect":
		return inspect(stdout, cfg, local.Fingerprint(), file, device)
	default:
		level := slog.LevelInfo
		if debug {
			level = slog.LevelDebug
		}
		return serve(ctx, config, cfg, local, stdout, slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level})))
	}
}

func pairing(cfg hubauth.Config) string {
	if cfg.PairingOperator == "" {
		return "disabled"
	}
	fp, _ := identity.ParseFingerprint(cfg.PairingOperator)
	return "enabled(" + fp.Short() + ")"
}
