package hsfzcapture

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"
)

const eventCost = 2048
const chunkLimit = 16384

// Recorder serializes lifecycle changes; callbacks never perform file I/O.
type Recorder struct {
	mu       sync.Mutex
	opt      Options
	snap     Snapshot
	epoch    uint64
	next     uint64
	run      *recording
	starting *starting
	open     func(string) (io.WriteCloser, error)
}
type starting struct {
	done     chan struct{}
	canceled bool
}

type recording struct {
	file       io.WriteCloser
	done       chan struct{}
	wake       chan struct{}
	queue      []event
	queued     int
	conns      map[uint64]*Connection
	stopping   bool
	stoppingAt time.Time
	epoch      uint64
	nextMarker uint64
}

// Connection belongs permanently to the capture epoch that admitted it.
type Connection struct {
	r       *Recorder
	run     *recording
	id      uint64
	meta    Metadata
	offsets [4]uint64
	ended   bool
	endedAt time.Time
	endCode string
	parsers [4]parser
}
type event struct {
	Type       string    `json:"type"`
	Version    int       `json:"version"`
	Capture    uint64    `json:"capture"`
	At         time.Time `json:"at"`
	Connection uint64    `json:"connection,omitempty"`
	Metadata   *Metadata `json:"metadata,omitempty"`
	Direction  Direction `json:"direction,omitempty"`
	Offset     uint64    `json:"offset,omitempty"`
	Data       string    `json:"data,omitempty"`
	Frame      *Frame    `json:"frame,omitempty"`
	Marker     *Marker   `json:"marker,omitempty"`
	Code       string    `json:"code,omitempty"`
	raw        []byte
	conn       *Connection
}

func bounded(n, def, max int) int {
	if n <= 0 {
		return def
	}
	if n > max {
		return max
	}
	return n
}

// New allocates no file or worker until Start accepts a recording.
func New(o Options) *Recorder {
	o.QueueBytes = bounded(o.QueueBytes, 4<<20, 16<<20)
	o.MaxConnections = bounded(o.MaxConnections, 128, 1024)
	o.RecentFrames = bounded(o.RecentFrames, 200, 200)
	if o.MaxFileBytes <= 0 {
		o.MaxFileBytes = 64 << 20
	}
	if o.MaxFileBytes > 1<<30 {
		o.MaxFileBytes = 1 << 30
	}
	return &Recorder{opt: o, open: openCapture}
}
func clean(s string) string {
	s = s[:min(len(s), 256)]
	var b strings.Builder
	for _, v := range s {
		if b.Len()+4 > 256 {
			break
		}
		if !unicode.IsControl(v) {
			b.WriteRune(v)
		}
	}
	return b.String()
}
func openCapture(path string) (io.WriteCloser, error) {
	if len(path) > 4096 || strings.ContainsRune(path, 0) || filepath.Base(path) == "." || filepath.Base(path) == ".." {
		return nil, errors.New("capture_path")
	}
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, errors.New("capture_path")
	}
	defer root.Close()
	f, err := root.OpenFile(filepath.Base(path), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, errors.New("capture_path")
	}
	return f, nil
}

// Start requires a new file. Its request context does not own a recording
// after success; Stop remains responsible for joining the file worker.
func (r *Recorder) Start(ctx context.Context, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	if r.run != nil || r.starting != nil {
		r.mu.Unlock()
		return errors.New("capture_busy")
	}
	pending := &starting{done: make(chan struct{})}
	r.starting = pending
	r.mu.Unlock()
	// Filesystem calls cannot be interrupted; pending remains joinable until close.
	f, err := r.open(path)
	r.mu.Lock()
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && pending.canceled {
		err = errors.New("capture_canceled")
	}
	if err != nil {
		r.mu.Unlock()
		if f != nil {
			f.Close()
		}
		r.mu.Lock()
		r.starting = nil
		close(pending.done)
		r.mu.Unlock()
		return err
	}
	r.epoch++
	s := &recording{file: f, done: make(chan struct{}), wake: make(chan struct{}, 1), conns: make(map[uint64]*Connection), epoch: r.epoch}
	r.snap = Snapshot{Active: true, Path: path, StartedAt: time.Now()}
	r.run = s
	r.starting = nil
	close(pending.done)
	r.mu.Unlock()
	go r.worker(s)
	return nil
}

// Stop joins pending file creation and the recording worker. A canceled context
// only abandons waiting; cleanup continues and a later Stop can join it.
// A nil result confirms joining, not successful recording: inspect Snapshot.Error
// and Snapshot.Incomplete for terminal recording failures or lost data.
func (r *Recorder) Stop(ctx context.Context) error {
	r.mu.Lock()
	if pending := r.starting; pending != nil {
		pending.canceled = true
		r.mu.Unlock()
		select {
		case <-pending.done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s := r.run
	if s == nil {
		r.mu.Unlock()
		return nil
	}
	if !s.stopping {
		s.stoppingAt = time.Now()
	}
	s.stopping = true
	r.snap.Active = false
	r.snap.Stopping = true
	r.signal(s)
	r.mu.Unlock()
	select {
	case <-s.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Snapshot copies the bounded recent index independently of file I/O.
func (r *Recorder) Snapshot() Snapshot {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.snap
	s.Recent = append([]Frame(nil), s.Recent...)
	s.RecentMarkers = append([]Marker(nil), s.RecentMarkers...)
	return s
}
func (r *Recorder) signal(s *recording) {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (r *Recorder) drop(s *recording) { r.snap.Incomplete = true; r.snap.DroppedEvents++; r.signal(s) }

// Begin returns nil while disabled or full; callers invoke it once at admission.
func (r *Recorder) Begin(m Metadata) *Connection {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.run
	if s == nil || s.stopping {
		return nil
	}
	if len(s.conns) >= r.opt.MaxConnections || s.queued+eventCost > r.opt.QueueBytes {
		r.drop(s)
		return nil
	}
	m.Network = clean(m.Network)
	m.Channel = clean(m.Channel)
	m.Local = clean(m.Local)
	m.Peer = clean(m.Peer)
	m.Target = clean(m.Target)
	r.next++
	c := &Connection{r: r, run: s, id: r.next, meta: m}
	s.conns[c.id] = c
	r.snap.ActiveConnections++
	s.queue = append(s.queue, event{Type: "connection_start", At: time.Now(), conn: c})
	s.queued += eventCost
	r.signal(s)
	return c
}
func directionIndex(d Direction) int {
	switch d {
	case ToVehicle:
		return 0
	case FromVehicle:
		return 1
	case ToOperator:
		return 2
	case FromOperator:
		return 3
	}
	return -1
}

var directions = [4]Direction{ToVehicle, FromVehicle, ToOperator, FromOperator}

// Record copies only bounded, queued bytes and never performs file I/O.
// Concurrent directions are independent; rejected bytes advance their offsets.
func (c *Connection) Record(d Direction, b []byte) {
	if c == nil {
		return
	}
	i := directionIndex(d)
	if i < 0 {
		return
	}
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	s := c.run
	if r.run != s || s.stopping || c.ended {
		return
	}
	observedAt := time.Now()
	r.snap.ObservedBytes += uint64(len(b))
	off := c.offsets[i]
	c.offsets[i] += uint64(len(b))
	for len(b) > 0 {
		n := min(len(b), chunkLimit)
		if s.queued+eventCost+n > r.opt.QueueBytes {
			r.drop(s)
			return
		}
		p := append([]byte(nil), b[:n]...)
		s.queue = append(s.queue, event{Type: "data", At: observedAt, conn: c, Direction: d, Offset: off, raw: p})
		s.queued += eventCost + n
		b = b[n:]
		off += uint64(n)
	}
	r.signal(s)
}

// End is idempotent and never closes the observed stream. Errors become a
// fixed category rather than arbitrary peer or filesystem text.
func (c *Connection) End(err error) {
	if c == nil {
		return
	}
	r := c.r
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.run != c.run || c.ended {
		return
	}
	c.ended = true
	c.endedAt = time.Now()
	if err != nil {
		c.endCode = "connection_error"
	}
	r.snap.ActiveConnections--
	r.signal(c.run)
}

// Datagram preserves a packet boundary as its own recorded connection.
func (r *Recorder) Datagram(m Metadata, d Direction, b []byte) {
	c := r.Begin(m)
	if c != nil {
		c.Record(d, b)
		c.End(nil)
	}
}
func (r *Recorder) write(s *recording, e event, terminal bool) bool {
	e.Version = 1
	e.Capture = s.epoch
	if e.At.IsZero() {
		e.At = time.Now()
	}
	if e.conn != nil {
		e.Connection = e.conn.id
		m := e.conn.meta
		e.Metadata = &m
	}
	e.conn = nil
	if e.raw != nil {
		e.Data = hex.EncodeToString(e.raw)
	}
	e.raw = nil
	b, _ := json.Marshal(e)
	b = append(b, '\n')
	r.mu.Lock()
	used := r.snap.BytesWritten
	failed := r.snap.Error != ""
	r.mu.Unlock()
	if failed && !terminal {
		return false
	}
	if !terminal && used+uint64(len(b))+512 > uint64(r.opt.MaxFileBytes) {
		r.fail(s, "file_limit")
		return false
	}
	if terminal && used+uint64(len(b)) > uint64(r.opt.MaxFileBytes) {
		return false
	}
	n, err := s.file.Write(b)
	r.mu.Lock()
	r.snap.BytesWritten += uint64(n)
	r.mu.Unlock()
	if err != nil || n != len(b) {
		r.fail(s, "write_failed")
		return false
	}
	return true
}
func (r *Recorder) fail(s *recording, code string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.snap.Error == "" {
		r.snap.Error = code
	}
	r.snap.Incomplete = true
	r.snap.Active = false
	r.snap.Stopping = true
	if !s.stopping {
		s.stoppingAt = time.Now()
	}
	s.stopping = true
	r.signal(s)
}
func (r *Recorder) worker(s *recording) {
	ok := r.write(s, event{Type: "capture_start", At: r.Snapshot().StartedAt}, false)
	for {
		r.mu.Lock()
		q := s.queue
		s.queue = nil
		stopping := s.stopping
		r.mu.Unlock()
		for j := range q {
			e := q[j]
			if ok {
				ok = r.write(s, e, false)
				if e.Type == "data" && ok {
					r.mu.Lock()
					r.snap.RecordedBytes += uint64(len(e.raw))
					r.mu.Unlock()
					i := directionIndex(e.Direction)
					e.conn.parsers[i].feed(r, s, e.conn, e.Direction, e.Offset, e.At, e.raw)
				}
				if e.Marker != nil && ok {
					r.mu.Lock()
					r.snap.Markers++
					if len(r.snap.RecentMarkers) == r.opt.RecentFrames {
						copy(r.snap.RecentMarkers, r.snap.RecentMarkers[1:])
						r.snap.RecentMarkers[len(r.snap.RecentMarkers)-1] = *e.Marker
					} else {
						r.snap.RecentMarkers = append(r.snap.RecentMarkers, *e.Marker)
					}
					r.mu.Unlock()
				}
			}
			cost := eventCost + len(e.raw)
			q[j] = event{}
			e = event{}
			r.mu.Lock()
			s.queued -= cost
			r.mu.Unlock()
		}
		q = nil
		r.mu.Lock()
		var ended []*Connection
		for id, c := range s.conns {
			if (c.ended || stopping) && len(s.queue) == 0 {
				// End cannot mutate an extracted connection after this lock is released.
				if !c.ended {
					c.ended = true
					c.endedAt = s.stoppingAt
					c.endCode = "capture_stopped"
					r.snap.ActiveConnections--
				}
				ended = append(ended, c)
				delete(s.conns, id)
			}
		}
		finished := stopping && len(s.queue) == 0
		r.mu.Unlock()
		for _, c := range ended {
			for i := range c.parsers {
				p := &c.parsers[i]
				p.finish(r, s, c, directions[i], c.offsets[i])
			}
			if ok {
				ok = r.write(s, event{Type: "connection_end", At: c.endedAt, conn: c, Code: c.endCode}, false)
			}
		}
		if finished {
			break
		}
		r.mu.Lock()
		pending := len(s.queue) > 0
		r.mu.Unlock()
		if !pending {
			<-s.wake
		}
	}
	r.mu.Lock()
	code := r.snap.Error
	if r.snap.Incomplete && code == "" {
		code = "incomplete"
	}
	r.mu.Unlock()
	if code != "write_failed" {
		if code != "" {
			r.write(s, event{Type: "incomplete", Code: code}, true)
		}
		r.write(s, event{Type: "capture_stop", Code: code}, true)
	}
	if err := s.file.Close(); err != nil {
		r.fail(s, "close_failed")
	}
	r.mu.Lock()
	r.snap.Active = false
	r.snap.Stopping = false
	r.snap.ActiveConnections = 0
	r.snap.StoppedAt = time.Now()
	r.run = nil
	close(s.done)
	r.mu.Unlock()
}
