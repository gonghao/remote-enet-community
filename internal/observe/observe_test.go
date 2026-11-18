package observe

import (
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLifecycleGenerationAndSnapshots(t *testing.T) {
	tracker := New(Options{TunnelHistory: 2, RTTSamples: 3, EventHistory: 2})
	generation := tracker.BeginSession()
	if got := tracker.Snapshot().Session.RTT; got.Samples != 0 || got.TotalSamples != 0 {
		t.Fatal(got)
	}
	h, err := tracker.BeginTunnel(generation, "127.0.0.1:6801", Outbound)
	if err != nil {
		t.Fatal(err)
	}
	h.AddIn(20)
	h.AddOut(30)
	for _, rtt := range []time.Duration{time.Millisecond, 2 * time.Millisecond, 3 * time.Millisecond, 4 * time.Millisecond} {
		if !tracker.AddRTT(generation, rtt) {
			t.Fatal("valid RTT rejected")
		}
	}
	if tracker.AddRTT(generation, -1) {
		t.Fatal("negative RTT accepted")
	}
	errClosed := errors.New("connection interrupted")
	h.End(errClosed)
	h.End(errors.New("duplicate"))
	h.AddIn(500)
	snapshot := tracker.Snapshot()
	if snapshot.ActiveConnections != 0 || len(snapshot.History) != 1 || snapshot.Totals != (Bytes{20, 30}) || snapshot.History[0].Error != errClosed.Error() {
		t.Fatalf("%+v", snapshot)
	}
	if rtt := snapshot.Session.RTT; rtt.Samples != 3 || rtt.TotalSamples != 4 || rtt.P50 != 3*time.Millisecond || rtt.P95 != 4*time.Millisecond || rtt.Latest != 4*time.Millisecond {
		t.Fatal(rtt)
	}
	snapshot.History[0].Target = "changed"
	snapshot.Events[0].Message = "changed"
	snapshot.LastError.Message = "changed"
	if again := tracker.Snapshot(); again.History[0].Target == "changed" || again.Events[0].Message == "changed" || again.LastError.Message == "changed" {
		t.Fatal("snapshot aliases tracker")
	}
	old, err := tracker.BeginTunnel(generation, "old", Inbound)
	if err != nil {
		t.Fatal(err)
	}
	old.AddIn(1)
	next := tracker.BeginSession()
	if next != generation+1 {
		t.Fatal(next)
	}
	tracker.EndSession(generation, errClosed)
	old.AddOut(500)
	old.End(errClosed)
	if tracker.AddRTT(generation, time.Second) {
		t.Fatal("stale RTT accepted")
	}
	if _, err := tracker.BeginTunnel(generation, "stale", Inbound); !errors.Is(err, ErrGeneration) {
		t.Fatal(err)
	}
	current := tracker.Snapshot()
	if !current.Session.Active || current.Session.RTT.Samples != 0 || current.Session.Bytes != (Bytes{}) || current.ActiveConnections != 0 || current.Totals != (Bytes{21, 30}) {
		t.Fatalf("%+v", current)
	}
	tracker.EndSession(next, nil)
	tracker.EndSession(next, errClosed)
	if tracker.Snapshot().Session.Active {
		t.Fatal("session did not end")
	}
}

func TestBoundsAndActiveSnapshotIsolation(t *testing.T) {
	tracker := New(Options{ActiveTunnels: 1, TunnelHistory: 2, EventHistory: 2, RTTSamples: 2})
	g := tracker.BeginSession()
	for range 20 {
		h, err := tracker.BeginTunnel(g, strings.Repeat("x", maxTextBytes*10), Inbound)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := tracker.BeginTunnel(g, "overflow", Inbound); !errors.Is(err, ErrLimit) {
			t.Fatal(err)
		}
		snapshot := tracker.Snapshot()
		snapshot.Active[0].Target = "changed"
		if again := tracker.Snapshot(); len(again.Active[0].Target) != maxTextBytes {
			t.Fatal("active snapshot aliases tracker")
		}
		h.End(nil)
		tracker.AddRTT(g, time.Millisecond)
		tracker.RecordEvent(g, "event", strings.Repeat("y", maxTextBytes*10), errors.New(strings.Repeat("z", maxTextBytes*10)))
	}
	s := tracker.Snapshot()
	if len(s.History) != 2 || s.History[0].ID != 19 || len(s.Events) != 2 || s.Session.RTT.Samples != 2 || s.Session.RTT.TotalSamples != 20 {
		t.Fatalf("%+v", s)
	}
	for _, event := range s.Events {
		if len(event.Message) > maxTextBytes || len(event.Error) > maxTextBytes {
			t.Fatal("unbounded message")
		}
	}
	if s.LastError == nil || len(s.LastError.Error) != maxTextBytes || !strings.HasPrefix(s.LastError.Error, "z") {
		t.Fatal("long message hid the actual error")
	}
	if _, err := tracker.BeginTunnel(g, "bad", "invalid"); err == nil {
		t.Fatal("invalid direction accepted")
	}
	clamped := New(Options{ActiveTunnels: 1 << 20, TunnelHistory: 1 << 20, RTTSamples: 1 << 20, EventHistory: 1 << 20, RateWindow: time.Hour})
	if o := clamped.options; o.ActiveTunnels != MaxActiveTunnels || o.TunnelHistory != MaxHistory || o.RTTSamples != MaxRTTSamples || o.EventHistory != MaxEvents || o.RateWindow != time.Minute {
		t.Fatal(o)
	}
}

func TestCurrentRateExpiresAndResets(t *testing.T) {
	tracker := New(Options{})
	now := time.Now()
	tracker.now = func() time.Time { return now }
	g := tracker.BeginSession()
	h, err := tracker.BeginTunnel(g, "target", Inbound)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(100 * time.Millisecond)
	h.AddIn(100)
	h.AddOut(200)
	if s := tracker.Snapshot(); math.Abs(s.Rate.InBytesPerSecond-1000) > 0.01 || math.Abs(s.Rate.OutBytesPerSecond-2000) > 0.01 {
		t.Fatal(s.Rate)
	}
	now = now.Add(2 * time.Second)
	if s := tracker.Snapshot(); s.Rate.InBytesPerSecond != 0 || s.Rate.OutBytesPerSecond != 0 || s.Rate.Window != time.Second {
		t.Fatal(s.Rate)
	}
	h.AddIn(1000)
	tracker.BeginSession()
	if s := tracker.Snapshot(); s.Rate.InBytesPerSecond != 0 || s.Session.Bytes != (Bytes{}) {
		t.Fatal(s)
	}
}

func TestConcurrentCountingAndIdempotentEnd(t *testing.T) {
	tracker := New(Options{})
	g := tracker.BeginSession()
	h, err := tracker.BeginTunnel(g, "target", Outbound)
	if err != nil {
		t.Fatal(err)
	}
	var writers sync.WaitGroup
	for range 24 {
		writers.Go(func() {
			for range 100 {
				h.AddIn(5)
				h.AddOut(7)
				tracker.AddRTT(g, time.Millisecond)
				tracker.RecordEvent(g, "progress", "", nil)
				_ = tracker.Snapshot()
			}
		})
	}
	writers.Wait()
	for range 24 {
		writers.Go(func() { h.End(nil) })
	}
	writers.Wait()
	s := tracker.Snapshot()
	if s.Totals != (Bytes{12000, 16800}) || s.ActiveConnections != 0 || len(s.History) != 1 {
		t.Fatal(s)
	}
}

func TestSessionEndClosesLeasesOnce(t *testing.T) {
	tracker := New(Options{})
	g := tracker.BeginSession()
	h, err := tracker.BeginTunnel(g, "target", Inbound)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() { h.AddIn(1); h.End(nil); tracker.EndSession(g, nil); tracker.Snapshot() })
	}
	workers.Wait()
	s := tracker.Snapshot()
	if s.Session.Active || s.ActiveConnections != 0 || len(s.History) != 1 || s.History[0].EndedAt.IsZero() {
		t.Fatal(s)
	}
}
