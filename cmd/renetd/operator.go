package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"runtime"
	"strconv"

	"remote-enet/internal/app"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/operator"
)

func runOperator(ctx context.Context, args []string, stderr io.Writer, logger *slog.Logger) (runErr error) {
	fs := flags("operator", stderr)
	stateDir := fs.String("state-dir", "", "operator state directory (required)")
	listen := fs.String("listen", "0.0.0.0:47000", "embedded hub listen host:port")
	bind := fs.String("bind", "", "local IPv4 address presented to diagnostic tools (required)")
	diagPort := fs.Int("diag-port", 6801, "local diagnostic TCP port; 0 chooses an ephemeral port")
	ctrlPort := fs.Int("ctrl-port", 6811, "local control TCP port; 0 chooses an ephemeral port")
	discPort := fs.Int("disc-port", 6811, "local discovery UDP port; 0 chooses an ephemeral port")
	remoteDiag := fs.Int("remote-diag-port", 6801, "vehicle gateway diagnostic TCP port")
	remoteCtrl := fs.Int("remote-ctrl-port", 6811, "vehicle gateway control TCP port")
	exclusive := fs.Bool("exclusive", runtime.GOOS == "windows", "restrict diagnostic and control TCP connections to one local process")
	autoMGU := fs.Bool("mgu-direct", false, "discover the MGU and enable per-session direct access; requires network configuration privileges")
	autoEVO := fs.Bool("evo-direct", false, "detect the EVO SSH endpoint and enable per-session direct access; requires network configuration privileges")
	capturePath := fs.String("capture-hsfz", "", "record new HSFZ connections to a new local JSONL file; off by default")
	opts := addRuntimeFlags(fs)
	if err := parsed(fs, args, stateDir); err != nil {
		return err
	}
	if *autoMGU && *autoEVO {
		return fmt.Errorf("--mgu-direct and --evo-direct are mutually exclusive")
	}
	if err := opts.validate(); err != nil {
		return err
	}
	bindAddr, err := ipv4(*bind, "--bind")
	if err != nil {
		return err
	}
	_, listenPort, err := net.SplitHostPort(*listen)
	if err != nil {
		return fmt.Errorf("--listen must be host:port")
	}
	n, err := strconv.Atoi(listenPort)
	if err != nil || port(n, "--listen port", false) != nil {
		return fmt.Errorf("--listen port must be in 1..65535")
	}
	for _, entry := range []struct {
		value int
		name  string
		zero  bool
	}{{*diagPort, "--diag-port", true}, {*ctrlPort, "--ctrl-port", true}, {*discPort, "--disc-port", true}, {*remoteDiag, "--remote-diag-port", false}, {*remoteCtrl, "--remote-ctrl-port", false}} {
		if err := port(entry.value, entry.name, entry.zero); err != nil {
			return err
		}
	}
	var capture *hsfzcapture.Recorder
	if *capturePath != "" {
		capture = hsfzcapture.New(hsfzcapture.Options{})
		if err := capture.Start(ctx, *capturePath); err != nil {
			return fmt.Errorf("operator: start HSFZ capture: %w", err)
		}
		defer func() {
			err := capture.Stop(context.Background())
			if s := capture.Snapshot(); s.Error != "" {
				err = errors.Join(err, fmt.Errorf("operator: HSFZ capture incomplete: %s", s.Error))
			}
			runErr = errors.Join(runErr, err)
		}()
	}
	r := app.New()
	return runRuntime(ctx, r, func(ctx context.Context) error {
		return r.RunOperator(ctx, app.OperatorConfig{
			StateDir: *stateDir, Listen: *listen, Bind: bindAddr, Exclusive: *exclusive, AllowLinkTest: opts.allowLinkTest,
			AutoMGU:              *autoMGU,
			AutoEVO:              *autoEVO,
			Capture:              capture,
			Ports:                operator.Ports{Diagnostic: *diagPort, Control: *ctrlPort, Discovery: *discPort},
			RemoteDiagnosticPort: uint16(*remoteDiag), RemoteControlPort: uint16(*remoteCtrl), Logger: logger,
		})
	}, *opts, logger)
}
