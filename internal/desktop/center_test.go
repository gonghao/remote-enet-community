package desktop

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
)

func TestCheckCenterUsesSavedSnapshotWithoutWriting(t *testing.T) {
	cfg := clientSettings()
	cfg.Center = settings.Center{ID: "garage", API: []string{"https://primary.invalid", "https://backup.invalid"}}
	writes := 0
	const version = "1.2.3"
	dir := t.TempDir()
	opts := Options{ConfigPath: filepath.Join(dir, "settings.json"), StateDir: dir, AppVersion: version}
	deps := production()
	deps.load = func(string) (settings.Config, error) { return cfg, nil }
	deps.save = func(string, settings.Config) error {
		writes++
		return nil
	}
	deps.checkCenter = func(_ context.Context, checked settings.Center, role, appVersion string) []center.EntryResult {
		if checked.ID != cfg.Center.ID || !slices.Equal(checked.API, cfg.Center.API) {
			t.Fatalf("unexpected center snapshot: %+v", checked)
		}
		if role != settings.RoleClient || appVersion != version {
			t.Fatalf("check received role %q and version %q", role, appVersion)
		}
		return []center.EntryResult{
			{Entry: checked.API[0], Status: center.Verified},
			{Entry: checked.API[1], Status: center.IDMismatch},
		}
	}
	c := fixedController(t, opts, deps)
	checked, results, err := c.CheckCenter(t.Context())
	if err != nil || len(results) != 2 || results[0].Status != center.Verified || results[1].Status != center.IDMismatch {
		t.Fatalf("check returned %v, %v", results, err)
	}
	checked.API[0] = "modified"
	state := c.Snapshot()
	if writes != 0 || !state.Config.Equal(cfg) || state.Notice != "" || state.Error != "" {
		t.Fatal("check modified configuration or controller state")
	}
}

func TestCheckCenterStopsOnControllerClose(t *testing.T) {
	cfg := clientSettings()
	cfg.Center = settings.Center{ID: "garage", API: []string{"https://primary.invalid"}}
	started := make(chan struct{})
	c := testController(t, cfg, func(d *dependencies) {
		d.checkCenter = func(ctx context.Context, _ settings.Center, _, _ string) []center.EntryResult {
			close(started)
			<-ctx.Done()
			return nil
		}
	})
	done := make(chan error, 1)
	go func() {
		_, _, err := c.CheckCenter(t.Context())
		done <- err
	}()
	await(t, started)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("check returned %v, want cancellation", err)
	}
}

func TestUnknownCenterReasonIsPrivate(t *testing.T) {
	const private = "ticket-canary-12345678"
	s := app.Snapshot{Role: session.RoleClient, LastError: private, Center: app.CenterSnapshot{State: centerflow.State{Phase: "ended", Reason: centerflow.Reason(private)}}}
	projected := displayRuntime(s)
	if projected.Center.Reason != "" || strings.Contains(projected.LastError, private) {
		t.Fatal("unrecognized remote reason reached public state")
	}
	if err := publicCenterError(&center.Error{Code: private}); err != ErrCenterOperation {
		t.Fatal("unrecognized remote reason reached the action result")
	}
}
