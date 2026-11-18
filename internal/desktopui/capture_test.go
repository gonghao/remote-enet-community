package desktopui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	renetui "remote-enet/internal/ui"
)

func TestCaptureStopHasReservedAdmissionAndCancelsPicker(t *testing.T) {
	s, posted := testShell(t)
	started := make(chan struct{}, 4)
	for _, action := range []renetui.Action{renetui.ActionRefresh, renetui.ActionSave, renetui.ActionTest} {
		s.submit(action, func(ctx context.Context) error {
			started <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		})
	}
	path := filepath.Join(t.TempDir(), "late.jsonl")
	s.chooseCaptureFile = func() (string, error) {
		t.Fatal("a canceled picker was opened")
		return path, nil
	}
	s.startCapture()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary action did not start")
		}
	}
	stopping := make(chan struct{})
	s.submit(renetui.ActionStopCapture, func(context.Context) error { close(stopping); return nil })
	s.submit(renetui.ActionStopCapture, func(context.Context) error { t.Error("duplicate stop was admitted"); return nil })
	select {
	case <-stopping:
	case <-time.After(time.Second):
		t.Fatal("capture stop was blocked by the full ordinary queue")
	}
	s.submit(renetui.ActionStop, func(context.Context) error { s.cancel(); return nil })
	s.workers.Wait()
	for range 7 { // picker, four ordinary completions, capture stop, runtime stop
		waitPosted(t, posted)
	}
	if s.captureStopPending || s.stopPending || s.captureCancel != nil || len(s.requests) != 0 {
		t.Fatal("completed capture actions retained their admission")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cancelled picker touched disk")
	}
}

func TestCapturePickerCancellationIsNormal(t *testing.T) {
	s, posted := testShell(t)
	s.chooseCaptureFile = func() (string, error) { return "", nil }
	s.startCapture()
	waitPosted(t, posted)
	waitPosted(t, posted)
	if s.captureCancel != nil || s.commandErr != "" || s.state.Capture.Active {
		t.Fatal("canceled picker was reported as a capture or an error")
	}
}

func TestLatePickerResultCannotStartAfterStop(t *testing.T) {
	s, posted := testShell(t)
	path := filepath.Join(t.TempDir(), "late.jsonl")
	s.chooseCaptureFile = func() (string, error) {
		// A native modal dialog pumps UI events; Stop can run before it returns.
		s.submit(renetui.ActionStop, func(context.Context) error { return nil })
		return path, nil
	}
	s.startCapture()
	waitPosted(t, posted)
	s.workers.Wait()
	waitPosted(t, posted)
	waitPosted(t, posted)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("late picker result started capture")
	}
	if s.commandErr != "" || s.captureCancel != nil {
		t.Fatal("canceled picker left an error or pending request")
	}
}
