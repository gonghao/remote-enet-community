package hsfzcapture

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

func start(t *testing.T, o Options) (*Recorder, string) {
	t.Helper()
	r := New(o)
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	if err := r.Start(context.Background(), path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { r.Stop(context.Background()) })
	return r, path
}
func readEvents(t *testing.T, path string) []event {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []event
	s := bufio.NewScanner(bytes.NewReader(b))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		var e event
		if err := json.Unmarshal(s.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestStreamsAndOriginalBytes(t *testing.T) {
	r, path := start(t, Options{})
	c := r.Begin(Metadata{Generation: 7, Network: "tcp", Channel: "diagnostic"})
	data := []byte{0, 0, 0, 3, 0, 1, 0xf1, 0x10, 0xff, 0, 0, 0, 0, 0xab, 0xcd}
	c.Record(ToVehicle, data[:2])
	c.Record(ToVehicle, data[2:9])
	c.Record(ToVehicle, data[9:])
	c.Record(FromVehicle, []byte{0, 0, 0, 0, 0, 0x11})
	c.End(errors.New("SECRET"))
	r.Datagram(Metadata{Network: "udp", Channel: "discovery"}, ToOperator, []byte{0, 0, 0, 0, 0, 0x11})
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	snap := r.Snapshot()
	if snap.Frames != 4 || snap.Incomplete || snap.RecordedBytes != 27 {
		t.Fatalf("%+v", snap)
	}
	var original []byte
	for _, e := range readEvents(t, path) {
		if e.Type == "data" && e.Connection == c.id && e.Direction == ToVehicle {
			b, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			if e.Offset != uint64(len(original)) {
				t.Fatal("offset")
			}
			original = append(original, b...)
		}
	}
	if !bytes.Equal(original, data) {
		t.Fatalf("raw %x", original)
	}
	if !snap.Recent[0].HasAddresses || snap.Recent[0].Source != 0xf1 {
		t.Fatal("addresses")
	}
	b, _ := os.ReadFile(path)
	if bytes.Contains(b, []byte("SECRET")) {
		t.Fatal("raw error leaked")
	}
	snap.Recent[0].Status = "mutated"
	if r.Snapshot().Recent[0].Status == "mutated" {
		t.Fatal("snapshot alias")
	}
}
func TestPartialOversizedAndPrefix(t *testing.T) {
	r, _ := start(t, Options{})
	c := r.Begin(Metadata{})
	c.Record(ToVehicle, []byte{0, 0})
	c.Record(FromVehicle, []byte{0xff, 0xff, 0xff, 0xff, 0, 1})
	c.Record(FromVehicle, []byte{0, 0, 0, 0, 0, 0x11})
	c.End(nil)
	p := r.Begin(Metadata{})
	b := make([]byte, 306)
	b[2] = 1
	b[3] = 44
	b[5] = 1
	p.Record(ToVehicle, b)
	p.End(nil)
	r.Stop(context.Background())
	s := r.Snapshot()
	statuses := map[string]int{}
	for _, f := range s.Recent {
		statuses[f.Status]++
		if f.BodyLength == 300 && (!f.PrefixTruncated || len(f.PrefixHex) != 512) {
			t.Fatal("prefix")
		}
	}
	if statuses["partial"] != 1 || statuses["oversized"] != 1 || statuses["complete"] != 1 {
		t.Fatalf("%+v", s)
	}
}
func TestDefaultDisabledAndEpoch(t *testing.T) {
	r := New(Options{})
	if r.Begin(Metadata{}) != nil {
		t.Fatal("enabled")
	}
	r.Datagram(Metadata{}, ToOperator, []byte{1})
	ctx, cancel := context.WithCancel(context.Background())
	p := filepath.Join(t.TempDir(), "one")
	if err := r.Start(ctx, p); err != nil {
		t.Fatal(err)
	}
	cancel()
	old := r.Begin(Metadata{})
	if old == nil {
		t.Fatal("request cancellation stopped recording")
	}
	r.Stop(context.Background())
	p2 := filepath.Join(t.TempDir(), "two")
	r.Start(context.Background(), p2)
	old.Record(ToVehicle, []byte{1})
	old.End(nil)
	r.Stop(context.Background())
	if r.Snapshot().ObservedBytes != 0 {
		t.Fatal("old epoch")
	}
}
func TestPaths(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "old")
	os.WriteFile(existing, []byte("original"), 0600)
	r := New(Options{})
	if r.Start(context.Background(), existing) == nil {
		t.Fatal("overwritten")
	}
	link := filepath.Join(dir, "link")
	if os.Symlink(existing, link) == nil {
		if r.Start(context.Background(), link) == nil {
			t.Fatal("followed link")
		}
	}
	alias := filepath.Join(dir, "alias")
	if os.Symlink(dir, alias) == nil {
		if err := r.Start(context.Background(), filepath.Join(alias, "new")); err != nil {
			t.Fatal(err)
		}
		r.Stop(context.Background())
	}
	b, _ := os.ReadFile(existing)
	if string(b) != "original" {
		t.Fatal("changed")
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	fail    bool
}

func (w *blockedWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	if w.fail {
		return 0, errors.New("SECRET disk error")
	}
	return len(b), nil
}
func (w *blockedWriter) Close() error { return nil }
func testBlocked(t *testing.T, fail bool) (*Recorder, *blockedWriter) {
	t.Helper()
	r := New(Options{QueueBytes: 4096, MaxConnections: 2})
	w := &blockedWriter{entered: make(chan struct{}), release: make(chan struct{}), fail: fail}
	s := &recording{file: w, done: make(chan struct{}), wake: make(chan struct{}, 1), conns: make(map[uint64]*Connection), epoch: 1}
	r.run = s
	r.snap.Active = true
	go r.worker(s)
	<-w.entered
	return r, w
}
func TestQueueGapStopAndSlowWriter(t *testing.T) {
	r, w := testBlocked(t, false)
	c := r.Begin(Metadata{})
	done := make(chan struct{})
	go func() { c.Record(ToVehicle, make([]byte, 8192)); c.End(nil); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("callback blocked")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(r.Stop(ctx), context.Canceled) {
		t.Fatal("stop cancellation")
	}
	if r.Start(context.Background(), filepath.Join(t.TempDir(), "busy")) == nil {
		t.Fatal("unjoined worker")
	}
	close(w.release)
	r.Stop(context.Background())
	s := r.Snapshot()
	if !s.Incomplete || s.DroppedEvents == 0 || s.ActiveConnections != 0 || len(s.Recent) != 1 || s.Recent[0].Status != "gap" {
		t.Fatalf("%+v", s)
	}
}
func TestWriterFailureAndFileBudget(t *testing.T) {
	r, w := testBlocked(t, true)
	c := r.Begin(Metadata{})
	c.Record(ToVehicle, []byte{1})
	close(w.release)
	r.Stop(context.Background())
	if r.Snapshot().Error != "write_failed" {
		t.Fatalf("%+v", r.Snapshot())
	}
	r, path := start(t, Options{MaxFileBytes: 1024})
	c = r.Begin(Metadata{})
	c.Record(ToVehicle, make([]byte, 1000))
	c.End(nil)
	r.Stop(context.Background())
	b, _ := os.ReadFile(path)
	if len(b) > 1024 || r.Snapshot().Error != "file_limit" {
		t.Fatalf("%d %+v", len(b), r.Snapshot())
	}
	readEvents(t, path)
}
func TestConcurrentStop(t *testing.T) {
	r, _ := start(t, Options{})
	c := r.Begin(Metadata{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Go(func() {
			for j := 0; j < 10; j++ {
				c.Record(ToVehicle, []byte{0, 0, 0, 0, 0, 0x11})
			}
			r.Stop(context.Background())
		})
	}
	wg.Wait()
	if r.Snapshot().Stopping {
		t.Fatal("still stopping")
	}
}

func TestCapacityRecentAndLimits(t *testing.T) {
	r, w := testBlocked(t, false)
	c := r.Begin(Metadata{})
	d := r.Begin(Metadata{})
	if d == nil || r.Begin(Metadata{}) != nil {
		t.Fatal("connection capacity")
	}
	c.End(nil)
	d.End(nil)
	close(w.release)
	r.Stop(context.Background())
	r, path := start(t, Options{RecentFrames: 2})
	c = r.Begin(Metadata{Channel: string(bytes.Repeat([]byte{'x'}, 1000)) + "\n"})
	for i := 0; i < 5; i++ {
		c.Record(ToVehicle, []byte{0, 0, 0, 0, 0, 0x11})
	}
	c.End(nil)
	r.Stop(context.Background())
	s := r.Snapshot()
	if len(s.Recent) != 2 || s.Frames != 5 || len(s.Recent[0].Channel) > 256 {
		t.Fatalf("%+v", s)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Windows inherits the parent directory ACL; mode bits do not describe it.
	if runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatalf("permissions %v", info.Mode())
	}
}

func TestStartIOIsIndependentAndStopJoins(t *testing.T) {
	r := New(Options{})
	entered, release, closed := make(chan struct{}), make(chan struct{}), make(chan struct{})
	r.open = func(string) (io.WriteCloser, error) {
		close(entered)
		<-release
		return &closeSignal{closed: closed}, nil
	}
	started := make(chan error, 1)
	go func() { started <- r.Start(context.Background(), "unused") }()
	<-entered
	callbacks := make(chan struct{})
	go func() {
		if r.Begin(Metadata{}) != nil {
			t.Error("admitted during start")
		}
		r.Datagram(Metadata{}, ToOperator, []byte{1})
		r.Snapshot()
		close(callbacks)
	}()
	select {
	case <-callbacks:
	case <-time.After(time.Second):
		t.Fatal("callbacks waited for file creation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(r.Stop(ctx), context.Canceled) {
		t.Fatal("stop failed to cancel wait")
	}
	if err := r.Start(context.Background(), "another"); err == nil {
		t.Fatal("overlapped pending start")
	}
	joined := make(chan struct{})
	go func() { r.Stop(context.Background()); close(joined) }()
	select {
	case <-joined:
		t.Fatal("stop did not join pending start")
	default:
	}
	close(release)
	if err := <-started; err == nil || err.Error() != "capture_canceled" {
		t.Fatalf("start result %v", err)
	}
	<-joined
	select {
	case <-closed:
	default:
		t.Fatal("canceled start leaked file")
	}
	if r.Snapshot().Active {
		t.Fatal("canceled start published epoch")
	}
	r.open = openCapture
	if err := r.Start(context.Background(), filepath.Join(t.TempDir(), "next")); err != nil {
		t.Fatal(err)
	}
	r.Stop(context.Background())
}

type closeSignal struct{ closed chan struct{} }

func (w *closeSignal) Write(b []byte) (int, error) { return len(b), nil }
func (w *closeSignal) Close() error                { close(w.closed); return nil }

type slowBuffer struct {
	bytes.Buffer
	entered, release chan struct{}
	once             sync.Once
}

func (w *slowBuffer) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.Buffer.Write(b)
}
func (w *slowBuffer) Close() error { return nil }
func TestObservedTimesSurviveWriterDelay(t *testing.T) {
	w := &slowBuffer{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(Options{})
	r.open = func(string) (io.WriteCloser, error) { return w, nil }
	if err := r.Start(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	<-w.entered
	before := time.Now()
	c := r.Begin(Metadata{})
	c.Record(ToVehicle, []byte{0, 0})
	afterFirst := time.Now()
	c.Record(ToVehicle, []byte{0, 0, 0, 0x11})
	c.End(nil)
	queuedBefore := time.Now()
	close(w.release)
	r.Stop(context.Background())
	scanner := bufio.NewScanner(bytes.NewReader(w.Bytes()))
	var first time.Time
	var frames int
	var ends int
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case "connection_start", "data":
			if e.At.Before(before) || e.At.After(queuedBefore) {
				t.Fatalf("observation time replaced: %+v", e)
			}
			if e.Type == "data" && first.IsZero() {
				first = e.At
			}
		case "connection_end":
			ends++
			if e.At.Before(before) || e.At.After(queuedBefore) || e.Code != "" {
				t.Fatalf("end observation time replaced: %+v", e)
			}
		case "frame":
			frames++
			if first.IsZero() || !e.Frame.At.Equal(first) || !e.At.Equal(first) || first.After(afterFirst) {
				t.Fatalf("frame lost first-byte timestamp: %+v first=%v", e, first)
			}
		}
	}
	if frames != 1 || ends != 1 {
		t.Fatalf("frames=%d ends=%d", frames, ends)
	}
}

// endBarrier holds the worker after map extraction, before the end event reaches disk.
type endBarrier struct {
	bytes.Buffer
	entered, release chan struct{}
}

func (w *endBarrier) Write(b []byte) (int, error) {
	var e event
	if err := json.Unmarshal(b, &e); err != nil {
		return 0, err
	}
	if e.Type == "connection_end" {
		close(w.entered)
		<-w.release
	}
	return w.Buffer.Write(b)
}
func (w *endBarrier) Close() error { return nil }
func TestStopFreezesActiveConnectionBeforeExtraction(t *testing.T) {
	w := &endBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	r := New(Options{})
	r.open = func(string) (io.WriteCloser, error) { return w, nil }
	if err := r.Start(context.Background(), "unused"); err != nil {
		t.Fatal(err)
	}
	c := r.Begin(Metadata{})
	before := time.Now()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Stop(ctx)
	after := time.Now()
	<-w.entered
	r.mu.Lock()
	at, code, ended := c.endedAt, c.endCode, c.ended
	_, retained := c.run.conns[c.id]
	r.mu.Unlock()
	if retained || !ended || code != "capture_stopped" || at.Before(before) || at.After(after) {
		t.Fatalf("end not frozen: %v %q %v retained=%v", ended, code, at, retained)
	}
	// This End races with the worker's end-event write in the original implementation.
	c.End(errors.New("late connection error"))
	r.mu.Lock()
	unchanged := c.endedAt.Equal(at) && c.endCode == code
	r.mu.Unlock()
	close(w.release)
	r.Stop(context.Background())
	if !unchanged {
		t.Fatal("late End mutated frozen state")
	}
	scanner := bufio.NewScanner(bytes.NewReader(w.Bytes()))
	var ends int
	for scanner.Scan() {
		var e event
		if err := json.Unmarshal(scanner.Bytes(), &e); err != nil {
			t.Fatal(err)
		}
		if e.Type == "connection_end" {
			ends++
			if e.Code != code || !e.At.Equal(at) {
				t.Fatalf("end event changed: %+v", e)
			}
		}
	}
	if ends != 1 {
		t.Fatalf("ends=%d", ends)
	}
}
