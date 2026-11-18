package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

type stabilityConfig struct {
	mode               string
	duration, interval time.Duration
	report             string
}

type resourceSample struct {
	Heap       uint64 `json:"heapAfterGc"`
	Goroutines int    `json:"goroutines"`
}

type stabilityReport struct {
	started       time.Time
	Schema        string            `json:"schema"`
	Mode          string            `json:"mode"`
	State         string            `json:"state"`
	GOOS          string            `json:"goos"`
	GOARCH        string            `json:"goarch"`
	RequestedMS   int64             `json:"requestedMs"`
	ElapsedMS     int64             `json:"elapsedMs"`
	StartedAt     time.Time         `json:"startedAt"`
	UpdatedAt     time.Time         `json:"updatedAt"`
	Transfers     uint64            `json:"transfers"`
	BytesEachWay  uint64            `json:"bytesEachWay"`
	Modes         map[string]uint64 `json:"faults"`
	PayloadHashes map[string]string `json:"payloadHashes"`
	Path          string            `json:"path"`
	Baseline      resourceSample    `json:"baseline"`
	Latest        resourceSample    `json:"latest"`
	Peak          resourceSample    `json:"peak"`
	AfterCleanup  resourceSample    `json:"afterCleanup"`
	WithinBudget  bool              `json:"resourcesWithinBudget"`
	WorkersJoined bool              `json:"workersJoined"`
	NoReplay      bool              `json:"noReplay"`
}

func TestManualStabilitySoak(t *testing.T) {
	if os.Getenv("RENET_MANUAL_STABILITY_SOAK") != "1" {
		t.Skip("explicit standalone stability runner required")
	}
	duration, err := time.ParseDuration(os.Getenv("RENET_STABILITY_DURATION"))
	if err != nil || duration < time.Minute || duration > 8*time.Hour {
		t.Fatal("stability duration must be 1m..8h")
	}
	interval, err := time.ParseDuration(os.Getenv("RENET_STABILITY_INTERVAL"))
	if err != nil || interval < time.Second || interval > time.Minute {
		t.Fatal("stability interval must be 1s..1m")
	}
	mode, report := os.Getenv("RENET_STABILITY_MODE"), os.Getenv("RENET_STABILITY_REPORT")
	if mode != "steady" && mode != "recovery" || !filepath.IsAbs(report) {
		t.Fatal("stability mode and absolute report path required")
	}
	runStability(t, stabilityConfig{mode: mode, duration: duration, interval: interval, report: report})
}

func sampleResources() resourceSample {
	// The sampling operation is explicit in the schema: retainable heap, not
	// transient allocation throughput. RSS is measured by the outside runner.
	runtime.GC()
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	return resourceSample{Heap: memory.HeapAlloc, Goroutines: runtime.NumGoroutine()}
}

func (r *stabilityReport) observe() {
	r.Latest = sampleResources()
	r.Peak.Heap = max(r.Peak.Heap, r.Latest.Heap)
	r.Peak.Goroutines = max(r.Peak.Goroutines, r.Latest.Goroutines)
	if r.Baseline == (resourceSample{}) {
		r.Baseline = r.Latest
	}
	r.WithinBudget = r.Latest.Heap <= r.Baseline.Heap+64<<20 && r.Latest.Goroutines <= r.Baseline.Goroutines+16
	r.ElapsedMS = time.Since(r.started).Milliseconds()
	r.UpdatedAt = time.Now().UTC()
}

func writeStabilityReport(path string, r stabilityReport) error {
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > 16<<10 {
		return fmt.Errorf("stability report exceeds budget")
	}
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return fmt.Errorf("stability report must be regular")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".stability-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, writeErr := f.Write(data)
	closeErr := f.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

func runStability(t *testing.T, cfg stabilityConfig) {
	t.Helper()
	ctx, stopSignal := signal.NotifyContext(t.Context(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	ctx, cancel := context.WithTimeout(ctx, cfg.duration+time.Minute)
	defer cancel()
	start := time.Now()
	r := stabilityReport{started: start, Schema: "stability-soak/v1", Mode: cfg.mode, State: "running", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		RequestedMS: cfg.duration.Milliseconds(), StartedAt: start.UTC(), WithinBudget: true,
		Modes: map[string]uint64{}, PayloadHashes: map[string]string{}}
	joined := false
	defer func() {
		r.UpdatedAt = time.Now().UTC()
		r.ElapsedMS = time.Since(r.started).Milliseconds()
		r.AfterCleanup = sampleResources()
		r.WorkersJoined = joined && !t.Failed()
		if ctx.Err() != nil {
			r.State = "canceled"
		} else if t.Failed() {
			r.State = "failed"
		}
		if err := writeStabilityReport(cfg.report, r); err != nil {
			t.Errorf("write stability report: %v", err)
		}
	}()
	if cfg.mode == "steady" {
		runSteadySoak(t, ctx, cfg, &r)
	} else {
		runRecoverySoak(t, ctx, cfg, &r)
	}
	joined = true
	if ctx.Err() == nil && !t.Failed() && r.WithinBudget {
		r.State = "passed"
		r.NoReplay = true
	}
}

func saveSoakProgress(t *testing.T, cfg stabilityConfig, r *stabilityReport) {
	t.Helper()
	r.observe()
	if err := writeStabilityReport(cfg.report, *r); err != nil {
		t.Fatal(err)
	}
	if !r.WithinBudget {
		t.Fatalf("resource budget exceeded: baseline=%+v latest=%+v", r.Baseline, r.Latest)
	}
}

func waitSoak(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func runSteadySoak(t *testing.T, ctx context.Context, cfg stabilityConfig, r *stabilityReport) {
	t.Helper()
	scope := &fixtureScope{T: t, ctx: ctx}
	defer scope.close()
	f := newL3FaultFixture(scope, ctx, false)
	c := f.dial(scope, 22)
	stopIO := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stopIO()
	defer c.Close()
	uploadHash := sha256.New()
	var allBytes uint64
	var payloads [2][]byte
	for i, size := range []int{1 << 20, 8 << 20} {
		payloads[i] = bytes.Repeat([]byte{0, 0xff, 0x42, 0x91}, size/4)
	}
	for {
		payload := payloads[r.Transfers%2]
		_ = c.SetDeadline(time.Now().Add(20 * time.Second))
		written := make(chan error, 1)
		go func() { _, err := io.Copy(c, bytes.NewReader(payload)); written <- err }()
		hash := sha256.New()
		n, readErr := io.CopyN(hash, c, int64(len(payload)))
		if readErr != nil {
			_ = c.Close()
		}
		writeErr := <-written
		if ctx.Err() != nil {
			return
		}
		expected := sha256.Sum256(payload)
		if readErr != nil || writeErr != nil || n != int64(len(payload)) || !bytes.Equal(hash.Sum(nil), expected[:]) {
			t.Fatalf("persistent transfer bytes=%d read=%v write=%v", n, readErr, writeErr)
		}
		if f.calls.Load() != 1 || f.l3.Snapshot().Accepted != 1 || f.ct.Err() != nil || f.ot.Err() != nil {
			t.Fatal("persistent connection was replaced or failed")
		}
		if f.link.shortPackets.Load() != 0 {
			t.Fatalf("fixture client queue dropped %d unconfigured packets", f.link.shortPackets.Load())
		}
		_, _ = uploadHash.Write(payload)
		allBytes += uint64(len(payload))
		r.Transfers++
		r.BytesEachWay = allBytes
		r.PayloadHashes[fmt.Sprint(n)] = hex.EncodeToString(expected[:])
		r.Path = string(f.ot.Path().Kind)
		if r.Transfers == 2 {
			r.started = time.Now()
			r.StartedAt = r.started.UTC()
			saveSoakProgress(t, cfg, r)
		}
		if r.Transfers > 2 {
			saveSoakProgress(t, cfg, r)
			if time.Since(r.started) >= cfg.duration {
				break
			}
			if !waitSoak(ctx, cfg.interval) {
				return
			}
		}
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if extra, err := io.Copy(io.Discard, c); err != nil || extra != 0 {
		t.Fatalf("persistent FIN bytes=%d: %v", extra, err)
	}
	select {
	case receipt := <-f.receipts:
		if receipt.err != nil || receipt.n != int64(allBytes) || !bytes.Equal(receipt.hash, uploadHash.Sum(nil)) {
			t.Fatal("persistent aggregate upload mismatch")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func runRecoverySoak(t *testing.T, ctx context.Context, cfg stabilityConfig, r *stabilityReport) {
	t.Helper()
	for {
		mode := []string{"cancel", "transport", "device"}[r.Transfers%3]
		size := []int{1 << 20, 8 << 20}[r.Transfers%2]
		var n int64
		var hash, path string
		func() {
			scope := &fixtureScope{T: t, ctx: ctx}
			defer scope.close()
			n, hash, path = runL3Recovery(scope, mode, size)
		}()
		if ctx.Err() != nil {
			return
		}
		r.Transfers++
		r.BytesEachWay += uint64(n)
		r.Modes[mode]++
		r.PayloadHashes[fmt.Sprint(n)] = hash
		r.Path = path
		if r.Transfers == 3 {
			r.started = time.Now()
			r.StartedAt = r.started.UTC()
			saveSoakProgress(t, cfg, r)
		}
		if r.Transfers > 3 {
			saveSoakProgress(t, cfg, r)
			if time.Since(r.started) >= cfg.duration {
				return
			}
			if !waitSoak(ctx, cfg.interval) {
				return
			}
		}
	}
}

func TestStabilitySmoke(t *testing.T) {
	for _, mode := range []string{"steady", "recovery"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "report.json")
			runStability(t, stabilityConfig{mode: mode, duration: time.Second, interval: 100 * time.Millisecond, report: path})
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var r stabilityReport
			if err := json.Unmarshal(data, &r); err != nil || r.State != "passed" || !r.WorkersJoined || !r.NoReplay || !r.WithinBudget || len(r.PayloadHashes) != 2 {
				t.Fatalf("incomplete smoke: %s %v", data, err)
			}
		})
	}
}
