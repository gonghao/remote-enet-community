package desktop

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/settings"
)

func TestCheckCenterUsesSavedSnapshotWithoutWriting(t *testing.T) {
	cfg := clientSettings()
	cfg.Center = settings.Center{ID: "garage", API: []string{"https://primary.invalid", "https://backup.invalid"}}
	writes := 0
	c := testController(t, cfg, func(d *dependencies) {
		d.save = func(string, settings.Config) error {
			writes++
			return nil
		}
		d.checkCenter = func(ctx context.Context, checked settings.Center) []center.EntryResult {
			if checked.ID != cfg.Center.ID || !slices.Equal(checked.API, cfg.Center.API) {
				t.Fatalf("unexpected center snapshot: %+v", checked)
			}
			return []center.EntryResult{
				{Entry: checked.API[0], Status: center.Verified},
				{Entry: checked.API[1], Status: center.IDMismatch},
			}
		}
	})
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
		d.checkCenter = func(ctx context.Context, _ settings.Center) []center.EntryResult {
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
