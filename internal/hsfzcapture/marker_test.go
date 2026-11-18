package hsfzcapture

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func markerEvents(t *testing.T, path string) []event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result []event
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		var e event
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		result = append(result, e)
	}
	return result
}

func TestMarkerOrdersWithBytesAndResetsPerRecording(t *testing.T) {
	r := New(Options{RecentFrames: 1})
	path := filepath.Join(t.TempDir(), "markers.jsonl")
	if _, err := r.Mark("", 1); !errors.Is(err, ErrNotRecording) {
		t.Fatal(err)
	}
	if err := r.Start(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	c := r.Begin(Metadata{Network: "tcp", Channel: "diagnostic", Generation: 7})
	c.Record(ToVehicle, []byte{0, 0, 0, 2, 0, 1})
	before := time.Now()
	m, err := r.Mark("  before tool action  ", 7)
	after := time.Now()
	if err != nil || m.Sequence != 1 || m.Label != "before tool action" || m.Generation != 7 || m.At.Before(before) || m.At.After(after) {
		t.Fatalf("marker admission: %+v %v", m, err)
	}
	c.Record(ToVehicle, []byte{0xf4, 0x10})
	auto, err := r.Mark("", 7)
	if err != nil || auto.Sequence != 2 || auto.Label != "Marker 2" {
		t.Fatalf("automatic marker: %+v %v", auto, err)
	}
	c.End(nil)
	if err := r.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	events := markerEvents(t, path)
	firstData, markIndex, secondData, frameIndex := -1, -1, -1, -1
	for i, e := range events {
		switch {
		case e.Type == "data" && e.Offset == 0:
			firstData = i
		case e.Type == "marker" && e.Marker.Sequence == 1:
			markIndex = i
			if e.Marker.Sequence != m.Sequence || e.Marker.Generation != m.Generation || e.Marker.Label != m.Label || !e.Marker.At.Equal(m.At) || !e.At.Equal(m.At) {
				t.Fatal("marker timestamp changed during writing")
			}
		case e.Type == "data" && e.Offset == 6:
			secondData = i
		case e.Type == "frame":
			frameIndex = i
		}
	}
	if firstData < 0 || markIndex <= firstData || secondData <= markIndex || frameIndex <= secondData || !events[frameIndex].Frame.At.Equal(events[firstData].At) {
		t.Fatal("marker corrupted byte order or the spanning frame boundary")
	}
	s := r.Snapshot()
	if s.Markers != 2 || len(s.RecentMarkers) != 1 || s.RecentMarkers[0].Sequence != 2 || s.ObservedBytes != 8 || s.RecordedBytes != 8 || s.Frames != 1 || s.Incomplete {
		t.Fatalf("marker accounting: %+v", s)
	}
	s.RecentMarkers[0].Label = "mutated"
	if r.Snapshot().RecentMarkers[0].Label == "mutated" {
		t.Fatal("snapshot exposed the marker list")
	}
	if err := r.Start(t.Context(), filepath.Join(t.TempDir(), "second.jsonl")); err != nil {
		t.Fatal(err)
	}
	m, err = r.Mark("", 8)
	if err != nil || m.Sequence != 1 {
		t.Fatal("marker numbering crossed capture epochs")
	}
	r.Stop(t.Context())
	if _, err := r.Mark("after stop", 8); !errors.Is(err, ErrNotRecording) {
		t.Fatal(err)
	}
}

func TestMarkerAdmissionIsIndependentOfDiskAndBounded(t *testing.T) {
	r, w := testBlocked(t, false)
	for i := range 2 {
		if m, err := r.Mark("", 1); err != nil || m.Sequence != uint64(i+1) {
			t.Fatalf("blocked writer blocked marker: %+v %v", m, err)
		}
	}
	if _, err := r.Mark("full", 1); !errors.Is(err, ErrMarkerFull) {
		t.Fatalf("full marker queue: %v", err)
	}
	if s := r.Snapshot(); s.Markers != 0 || len(s.RecentMarkers) != 0 || s.DroppedEvents != 1 {
		t.Fatalf("unwritten markers were claimed persisted: %+v", s)
	}
	close(w.release)
	r.Stop(t.Context())
	if r.Snapshot().Markers != 2 {
		t.Fatal("admitted markers were not drained")
	}
}

func TestMarkerLabelsAndStopRace(t *testing.T) {
	r := New(Options{})
	for _, label := range []string{"private\nlabel", "private\x00label", string([]byte{0xff}), strings.Repeat("x", 257)} {
		if _, err := r.Mark(label, 1); !errors.Is(err, ErrMarkerLabel) || strings.Contains(err.Error(), "private") {
			t.Fatalf("invalid marker label: %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "race.jsonl")
	if err := r.Start(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Uint64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range 8 {
		wg.Go(func() {
			<-start
			for range 10 {
				if _, err := r.Mark("", 1); err == nil {
					accepted.Add(1)
				} else if !errors.Is(err, ErrNotRecording) {
					t.Errorf("mark during stop: %v", err)
				}
			}
		})
	}
	close(start)
	r.Stop(context.Background())
	wg.Wait()
	if r.Snapshot().Markers != accepted.Load() {
		t.Fatal("Stop lost an admitted marker")
	}
	count := uint64(0)
	for _, e := range markerEvents(t, path) {
		if e.Type == "marker" {
			count++
		}
	}
	if count != accepted.Load() {
		t.Fatal("file and marker admissions differ")
	}
}
