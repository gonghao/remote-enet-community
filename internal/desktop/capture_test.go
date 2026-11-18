package desktop

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/session"
)

func startCaptureController(t *testing.T, f *controlledRuntime) (*Controller, runCall) {
	t.Helper()
	c := testController(t, operatorSettings(), func(d *dependencies) { d.newRuntime = func() appRuntime { return f } })
	if err := c.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	return c, await(t, f.started)
}

func captureBytes(t *testing.T, path string) map[hsfzcapture.Direction][]byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var last string
	streams := map[hsfzcapture.Direction][]byte{}
	dec := json.NewDecoder(bytes.NewReader(b))
	for dec.More() {
		var e struct {
			Type      string
			Direction hsfzcapture.Direction
			Data      string
		}
		if err := dec.Decode(&e); err != nil {
			t.Fatal(err)
		}
		last = e.Type
		if e.Type == "data" {
			p, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			streams[e.Direction] = append(streams[e.Direction], p...)
		}
	}
	if last != "capture_stop" {
		t.Fatalf("file was not drained and closed: last=%q", last)
	}
	return streams
}

func TestCaptureRunScopeSnapshotAndDraining(t *testing.T) {
	f := newRuntime()
	f.release = make(chan struct{})
	c, call := startCaptureController(t, f)
	if call.operator.Capture == nil || call.operator.Capture.Begin(hsfzcapture.Metadata{}) != nil {
		t.Fatal("capture must be installed but off by default")
	}
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.jsonl"), filepath.Join(dir, "second.jsonl")
	request, cancel := context.WithCancel(t.Context())
	if err := c.StartCapture(request, first); err != nil {
		t.Fatal(err)
	}
	cancel()
	r := call.operator.Capture
	conn := r.Begin(hsfzcapture.Metadata{Network: "tcp", Channel: "diagnostic", Generation: 3})
	if conn == nil {
		t.Fatal("request cancellation ended the recording")
	}
	payload := hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}
	wire, _ := payload.MarshalBinary()
	conn.Record(hsfzcapture.ToVehicle, wire)
	conn.Record(hsfzcapture.FromVehicle, wire)
	s := stateWhere(t, c, func(s State) bool { return s.Capture.Frames == 2 })
	s.Capture.Recent[0].PrefixHex = "changed"
	if c.Snapshot().Capture.Recent[0].PrefixHex == "changed" {
		t.Fatal("snapshot exposed mutable capture storage")
	}
	if err := c.StopCapture(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !c.Snapshot().Running || c.Snapshot().Capture.Active {
		t.Fatal("stopping capture affected the runtime")
	}
	got := captureBytes(t, first)
	if !bytes.Equal(got[hsfzcapture.ToVehicle], wire) || !bytes.Equal(got[hsfzcapture.FromVehicle], wire) {
		t.Fatalf("captured bytes=%x", got)
	}
	if err := c.StartCapture(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	conn.Record(hsfzcapture.ToVehicle, []byte("old capture"))
	conn.End(nil)
	next := r.Begin(hsfzcapture.Metadata{Network: "tcp", Channel: "control", Generation: 4})
	next.Record(hsfzcapture.ToVehicle, wire)
	if err := c.Stop(canceledContext()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	await(t, f.canceled)
	if !r.Snapshot().Active {
		t.Fatal("capture stopped before the runtime joined its forwarding workers")
	}
	next.Record(hsfzcapture.FromVehicle, wire)
	next.End(nil)
	close(f.release)
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	got = captureBytes(t, second)
	if !bytes.Equal(got[hsfzcapture.ToVehicle], wire) || !bytes.Equal(got[hsfzcapture.FromVehicle], wire) {
		t.Fatalf("capture epochs mixed: %x", got)
	}
	s = c.Snapshot()
	if s.Running || s.Capture.Active || s.Capture.Stopping || s.Capture.StoppedAt.IsZero() {
		t.Fatal("Stop returned before capture cleanup")
	}
}

func TestCaptureFailuresDoNotStopRuntimeOrOverwriteFiles(t *testing.T) {
	f := newRuntime()
	c, call := startCaptureController(t, f)
	path := filepath.Join(t.TempDir(), "renet1-PRIVATE.jsonl")
	if err := os.WriteFile(path, []byte("existing evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := c.StartCapture(t.Context(), path); err == nil {
		t.Fatal("existing capture was overwritten")
	}
	b, _ := os.ReadFile(path)
	s := c.Snapshot()
	if string(b) != "existing evidence" || !s.Running || s.Capture.Active || call.operator.Capture.Snapshot().Active {
		t.Fatal("capture failure modified the file or stopped the runtime")
	}
	if strings.Contains(s.Error+strings.Join(s.Events, " "), "PRIVATE") {
		t.Fatal("capture path entered ordinary error history")
	}
	if err := c.StartCapture(canceledContext(), filepath.Join(t.TempDir(), "cancelled.jsonl")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "close.jsonl")
	if err := c.StartCapture(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	captureBytes(t, path)
	if c.Snapshot().Capture.Active || c.Snapshot().Capture.Stopping {
		t.Fatal("controller Close abandoned the file")
	}
}

func TestCaptureRequiresRunningOperator(t *testing.T) {
	for _, cfg := range []bool{false, true} {
		settings := clientSettings()
		if cfg {
			settings = operatorSettings()
		}
		c := testController(t, settings, func(*dependencies) {})
		path := filepath.Join(t.TempDir(), "not-created.jsonl")
		if err := c.StartCapture(t.Context(), path); err == nil {
			t.Fatal("idle capture admitted")
		}
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("rejected capture touched disk")
		}
	}
}

func TestCaptureMarkerUsesCurrentRuntimeAndOwnsSnapshots(t *testing.T) {
	f := newRuntime()
	c, _ := startCaptureController(t, f)
	path := filepath.Join(t.TempDir(), "markers.jsonl")
	if err := c.StartCapture(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	// The runtime can publish a new generation before the coalescing pump reads it.
	f.mu.Lock()
	f.state.Generation = 9
	f.state.Role = session.RoleOperator
	f.mu.Unlock()
	m, err := c.MarkCapture(t.Context(), "PRIVATE_ANNOTATION")
	if err != nil || m.Generation != 9 {
		t.Fatalf("marker used an obsolete displayed generation: %+v %v", m, err)
	}
	s := stateWhere(t, c, func(s State) bool { return s.Capture.Markers == 1 })
	s.Capture.RecentMarkers[0].Label = "mutated"
	if c.Snapshot().Capture.RecentMarkers[0].Label == "mutated" || strings.Contains(strings.Join(s.Events, " "), "PRIVATE_ANNOTATION") {
		t.Fatal("marker storage was borrowed or entered ordinary events")
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MarkCapture(t.Context(), "late"); !errors.Is(err, hsfzcapture.ErrNotRecording) {
		t.Fatal(err)
	}
}
