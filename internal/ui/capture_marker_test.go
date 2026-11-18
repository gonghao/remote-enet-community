package ui

import (
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

func TestMarkerButtonStaysAccessibleAndUsesDraft(t *testing.T) {
	var labels []string
	accepted := true
	v := New(operatorConfig(), Actions{MarkCapture: func(label string) bool {
		labels = append(labels, label)
		return accepted
	}})
	state := State{Running: true, Capture: hsfzcapture.Snapshot{Active: true, StartedAt: time.Unix(1, 0)}, CaptureMarkerShortcut: "Ctrl+Shift+M"}
	v.SetState(state)
	tt := ui.NewTester(v.Render, 900, 620)
	click(t, tt, "Insert HSFZ marker")
	if len(labels) != 1 || labels[0] != "" {
		t.Fatal("quick marker did not use automatic numbering")
	}
	openCapture(t, tt)
	v.captureMarkerLabel = "Before reading VIN"
	tt.Frame()
	click(t, tt, "Insert HSFZ marker")
	if len(labels) != 2 || labels[1] != "Before reading VIN" || v.captureMarkerLabel != "" {
		t.Fatal("marker did not consume the optional label")
	}
	accepted = false
	v.captureMarkerLabel = "Keep this label"
	tt.Frame()
	click(t, tt, "Insert HSFZ marker")
	if v.captureMarkerLabel != "Keep this label" {
		t.Fatal("rejected marker discarded its label")
	}
	for _, page := range []page{sessionPage, settingsPage, logPage, capturePage} {
		v.page = page
		tt.Frame()
		button, ok := tt.Find("Insert HSFZ marker")
		if !ok || button.Y >= 100 {
			t.Fatalf("marker action is not at the top on page %d", page)
		}
	}
	state.Stopping = true
	v.SetState(state)
	if v.InsertCaptureMarker() {
		t.Fatal("marker admitted during runtime stop")
	}
}

func TestMarkersFilterFrameStartTimesAndOwnTheirSnapshot(t *testing.T) {
	boundary := time.Unix(10, 0)
	v := New(operatorConfig(), Actions{})
	state := State{Capture: hsfzcapture.Snapshot{
		StartedAt: time.Unix(1, 0), Markers: 1,
		RecentMarkers: []hsfzcapture.Marker{{Sequence: 1, At: boundary, Label: "Before action", Generation: 4}},
		Recent: []hsfzcapture.Frame{
			{Sequence: 1, At: boundary.Add(-time.Millisecond), Status: "complete"},
			{Sequence: 2, At: boundary, Status: "complete"},
			{Sequence: 3, At: boundary.Add(time.Millisecond), Status: "complete"},
		},
	}}
	v.SetState(state)
	state.Capture.RecentMarkers[0].Label = "mutated"
	if v.state.Capture.RecentMarkers[0].Label != "Before action" {
		t.Fatal("view borrowed marker storage")
	}
	tt := ui.NewTester(v.Render, 1080, 1000)
	openCapture(t, tt)
	click(t, tt, "Show frames after marker 1")
	tt.Frame()
	if _, ok := tt.Find("Select capture frame 1"); ok {
		t.Fatal("frame starting before the marker passed the time filter")
	}
	for _, name := range []string{"Select capture frame 2", "Select capture frame 3"} {
		if _, ok := tt.Find(name); !ok {
			t.Fatal("marker time filter discarded a matching frame")
		}
	}
	click(t, tt, "Clear marker time filter")
	tt.Frame()
	if _, ok := tt.Find("Select capture frame 1"); !ok {
		t.Fatal("clearing marker filter did not restore earlier frames")
	}
	click(t, tt, "Show frames after marker 1")
	state.Capture.StartedAt = time.Unix(2, 0)
	v.SetState(state)
	if v.captureAfterMarker != nil {
		t.Fatal("marker filter crossed recording epochs")
	}
}

func TestClientCannotInsertCaptureMarkers(t *testing.T) {
	v := New(settings.Default(), Actions{MarkCapture: func(string) bool { t.Fatal("client marker action"); return true }})
	v.SetState(State{Running: true, Capture: hsfzcapture.Snapshot{Active: true}})
	tt := ui.NewTester(v.Render, 820, 640)
	if _, ok := tt.Find("Insert HSFZ marker"); ok || v.InsertCaptureMarker() {
		t.Fatal("client exposed Operator marker controls")
	}
}
