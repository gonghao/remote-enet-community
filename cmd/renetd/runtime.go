package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/linktest"
)

type runtimeFlags struct {
	centerOutput   io.Writer
	allowLinkTest  bool
	connectionTest bool
	testTimeout    time.Duration
	statusInterval time.Duration
}

func addRuntimeFlags(fs *flag.FlagSet) *runtimeFlags {
	opts := &runtimeFlags{}
	fs.BoolVar(&opts.allowLinkTest, "allow-link-test", true, "allow negotiated peer-only connection tests")
	fs.BoolVar(&opts.connectionTest, "connection-test", false, "test the peer link once per new session; never connects to the vehicle")
	fs.DurationVar(&opts.testTimeout, "connection-test-timeout", 10*time.Second, "peer connection test budget (1ms..15s)")
	fs.DurationVar(&opts.statusInterval, "status-interval", 5*time.Second, "status log interval; 0 disables periodic status")
	return opts
}

func (o runtimeFlags) validate() error {
	if o.statusInterval < 0 {
		return fmt.Errorf("--status-interval must not be negative")
	}
	if o.connectionTest && !o.allowLinkTest {
		return fmt.Errorf("--connection-test requires --allow-link-test")
	}
	if _, err := (linktest.Options{Timeout: o.testTimeout}).Normalize(); err != nil {
		return fmt.Errorf("--connection-test-timeout: %w", err)
	}
	return nil
}

// runRuntime joins both workers before returning, including on configuration
// errors. The bounded test queue replaces obsolete generations, never sessions.
func runRuntime(ctx context.Context, r *app.Runtime, run func(context.Context) error, opts runtimeFlags, logger *slog.Logger) error {
	if opts.statusInterval == 0 && !opts.connectionTest && opts.centerOutput == nil {
		return run(ctx)
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	states := r.Subscribe(workerCtx)
	tests := make(chan uint64, 1)
	var wg sync.WaitGroup
	if opts.centerOutput != nil {
		updates := r.Subscribe(workerCtx)
		wg.Go(func() { centerConsole(workerCtx, r, updates, opts.centerOutput) })
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		var tick <-chan time.Time
		if opts.statusInterval > 0 {
			timer := time.NewTicker(opts.statusInterval)
			defer timer.Stop()
			tick = timer.C
		}
		var seen uint64
		for {
			select {
			case <-workerCtx.Done():
				return
			case state, ok := <-states:
				if !ok {
					return
				}
				if opts.connectionTest && state.Phase == app.PhaseConnected && state.Generation > seen {
					seen = state.Generation
					select {
					case <-tests:
					default:
					}
					tests <- seen
				}
			case <-tick:
				logStatus(logger, r.Snapshot())
			}
		}
	}()
	if opts.connectionTest {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-workerCtx.Done():
					return
				case generation := <-tests:
					testGeneration(workerCtx, r, generation, linktest.Options{Timeout: opts.testTimeout}, logger)
				}
			}
		}()
	}
	err := run(ctx)
	if opts.centerOutput != nil && ctx.Err() != nil && r.Snapshot().Center.SessionShort != "" {
		endCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		endErr := r.EndReception(endCtx)
		stop()
		if endErr != nil {
			err = fmt.Errorf("could not end center reception: %w", endErr)
		}
	}
	cancel()
	wg.Wait()
	if opts.centerOutput != nil {
		s := r.Snapshot().Center
		logger.Info("center reception finished", "phase", s.Phase, "session", s.SessionShort, "reason", s.Reason)
	}
	if opts.statusInterval > 0 {
		logStatus(logger, r.Snapshot())
	}
	return err
}

func logStatus(logger *slog.Logger, s app.Snapshot) {
	logger.Info("runtime status",
		"role", s.Role, "phase", s.Phase, "generation", s.Generation,
		"rtt_valid", s.RTTValid, "rtt", s.RTT, "rtt_samples", s.Metrics.Session.RTT.TotalSamples,
		"payload_bytes_in", s.Metrics.Session.Bytes.In, "payload_bytes_out", s.Metrics.Session.Bytes.Out,
		"total_payload_bytes_in", s.Metrics.Totals.In, "total_payload_bytes_out", s.Metrics.Totals.Out,
		"in_bytes_per_second", s.Metrics.Rate.InBytesPerSecond, "out_bytes_per_second", s.Metrics.Rate.OutBytesPerSecond,
		"active_connections", s.Metrics.ActiveConnections, "metrics_incomplete", s.MetricsIncomplete, "path", s.Path.Kind,
		"exclusive_enabled", s.Exclusive.Enabled, "owner_pid", s.Exclusive.Owner.PID, "owner_created", s.Exclusive.Owner.Created, "owner_connections", s.Exclusive.Active,
		"last_error", s.LastError, "next_retry", s.NextRetry)
	if s.Role == "operator" && s.Direct.Phase != "" && s.Direct.Phase != app.DirectDisabled {
		logger.Info("Head unit direct link", "generation", s.Generation, "phase", s.Direct.Phase,
			"address", s.Direct.Address.String(), "local", s.Direct.Local.String(), "adapter", s.Direct.Adapter,
			"error", s.Direct.LastError)
	}
}

func testGeneration(ctx context.Context, r *app.Runtime, generation uint64, options linktest.Options, logger *slog.Logger) {
	result, err := r.TestLinkForGeneration(ctx, generation, options)
	status := "ok"
	switch {
	case errors.Is(err, linktest.ErrUnsupported):
		status = "unsupported"
	case result.TimedOut:
		status = "timeout"
	case errors.Is(err, context.Canceled), errors.Is(err, app.ErrNotConnected):
		status = "canceled"
	case err != nil:
		status = "error"
	}
	attrs := []any{"generation", generation, "status", status, "requested_samples", result.RequestedSamples, "samples", result.Samples,
		"rtt_valid", result.Samples > 0, "rtt_p50", result.RTTP50, "rtt_p95", result.RTTP95,
		"bytes_sent", result.BytesSent, "bytes_received", result.BytesReceived, "echoed_bytes", result.EchoedBytes,
		"duration", result.Duration, "payload_duration", result.PayloadDuration, "echo_bytes_per_second", result.EchoBytesPerSecond,
		"timed_out", result.TimedOut, "interrupted", result.Interrupted}
	if err != nil {
		logger.Warn("connection test failed", append(attrs, "error", err)...)
	} else {
		logger.Info("connection test completed", attrs...)
	}
}
