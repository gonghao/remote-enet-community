package desktopui

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/settings"
	renetui "remote-enet/internal/ui"
)

func markerShell(t *testing.T) (*shell, chan func()) {
	t.Helper()
	s, posted := testShell(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	listener.Close()
	cfg := s.controller.Snapshot().Config
	cfg.Role = settings.RoleOperator
	cfg.Operator.Listen, cfg.Operator.Endpoint, cfg.Operator.Bind = endpoint, endpoint, "127.0.0.1"
	cfg.Operator.Exclusive = false
	if err := s.controller.Save(t.Context(), cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.controller.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	listening := false
	for state := range s.controller.Subscribe(ctx) {
		if state.Runtime.Phase == app.PhaseListening {
			listening = true
			break
		}
	}
	if !listening {
		t.Fatal("Operator did not start listening")
	}
	if err := s.controller.StartCapture(t.Context(), filepath.Join(t.TempDir(), "markers.jsonl")); err != nil {
		t.Fatal(err)
	}
	s.product, _ = productForRole(settings.RoleOperator)
	s.state = s.controller.Snapshot()
	s.view = renetui.New(cfg, renetui.Actions{MarkCapture: s.markCapture})
	return s, posted
}

func TestMarkerHotkeyBypassesActionQueueAndKeepsLabelsPrivate(t *testing.T) {
	s, posted := markerShell(t)
	var trigger func()
	var registrations, releases int
	s.registerCaptureShortcut = func(acc string, keys func(string), pressed func()) error {
		if acc != captureMarkerAccelerator {
			t.Fatal("wrong shortcut")
		}
		registrations++
		trigger = pressed
		return nil
	}
	s.unregisterCaptureShortcut = func(string) { releases++ }
	s.applyState()
	s.applyState()
	if trigger == nil || registrations != 1 || s.captureShortcutKeys == "" {
		t.Fatal("shortcut registration was repeated or unavailable")
	}
	firstTrigger := trigger
	started, release := make(chan struct{}, 4), make(chan struct{})
	for _, action := range []renetui.Action{renetui.ActionRefresh, renetui.ActionSave, renetui.ActionInvite, renetui.ActionTest} {
		s.submit(action, func(ctx context.Context) error {
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary action did not start")
		}
	}
	trigger()
	if s.commandNotice != "Marker 1 queued." || len(s.requests) != 4 {
		t.Fatal("global marker waited for the ordinary action queue")
	}
	private := "PRIVATE_MARKER_LABEL"
	if !s.markCapture(private) {
		t.Fatal("explicit marker failed")
	}
	var report string
	s.copyText = func(value string) { report = value }
	s.copyDiagnostics()
	if report == "" || strings.Contains(report, private) || strings.Contains(report, "RecentMarkers") {
		t.Fatal("marker payload entered anonymous diagnostics")
	}
	if strings.Contains(strings.Join(s.controller.Snapshot().Events, " "), private) {
		t.Fatal("annotation entered ordinary event history")
	}
	close(release)
	s.workers.Wait()
	for range 4 {
		waitPosted(t, posted)
	}
	if err := s.controller.StopCapture(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.state = s.controller.Snapshot()
	s.applyState()
	if releases != 1 || s.captureShortcutRegistered || s.state.Capture.Markers != 2 {
		t.Fatal("shortcut or admitted markers survived Stop incorrectly")
	}
	if err := s.controller.StartCapture(t.Context(), filepath.Join(t.TempDir(), "second.jsonl")); err != nil {
		t.Fatal(err)
	}
	s.state = s.controller.Snapshot()
	s.applyState()
	if registrations != 2 {
		t.Fatal("new recording did not bind its shortcut")
	}
	previousNotice := s.commandNotice
	firstTrigger()
	if s.commandNotice != previousNotice {
		t.Fatal("an old shortcut callback entered a replacement recording")
	}
	s.quitting = true
	s.releaseCaptureShortcut()
	trigger()
	if s.controller.Snapshot().Capture.Markers != 0 || releases != 2 {
		t.Fatal("late shortcut callback recorded during Quit")
	}
}

func TestMarkerShortcutConflictLeavesButtonAvailable(t *testing.T) {
	s, _ := markerShell(t)
	var calls int
	s.registerCaptureShortcut = func(string, func(string), func()) error {
		calls++
		return errors.New("PRIVATE operating system error")
	}
	s.unregisterCaptureShortcut = func(string) { t.Fatal("unregistered an unowned shortcut") }
	for range 3 {
		s.applyState()
	}
	if calls != 1 || !s.captureShortcutUnavailable || s.captureShortcutKeys != "" || strings.Contains(s.commandErr, "PRIVATE") {
		t.Fatal("shortcut conflict was retried or leaked raw text")
	}
	if !s.view.InsertCaptureMarker() {
		t.Fatal("shortcut failure disabled the marker button")
	}
}
