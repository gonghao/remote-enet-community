// Package observe aggregates bounded, local measurements without owning I/O.
package observe

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	MaxActiveTunnels = 4096
	MaxHistory       = 4096
	MaxRTTSamples    = 1024
	MaxEvents        = 1024
	maxTextBytes     = 512
	rateBuckets      = 20
)

var (
	ErrGeneration = errors.New("observe: inactive session generation")
	ErrLimit      = errors.New("observe: active tunnel limit reached")
)

// Direction identifies the opener independently of byte accounting.
type Direction string

const (
	// Inbound and Outbound describe who opened the tunnel, not byte direction.
	Inbound  Direction = "inbound"
	Outbound Direction = "outbound"
)

// Options uses defaults for nonpositive values and clamps to hard limits.
type Options struct {
	ActiveTunnels int
	TunnelHistory int
	RTTSamples    int
	EventHistory  int
	// RateWindow is rounded to 20 buckets, between 100ms and one minute.
	RateWindow time.Duration
}

// Bytes are measured from the local endpoint: In is remote to local.
type Bytes struct{ In, Out uint64 }

// Rate approximates the trailing window with 20 buckets; it is not an average
// over the session lifetime. Window reports the actual denominator.
type Rate struct {
	InBytesPerSecond  float64
	OutBytesPerSecond float64
	Window            time.Duration
}

// RTT has no measurement when Samples is zero. Percentiles use nearest rank.
type RTT struct {
	Direction    string
	Samples      int
	TotalSamples uint64
	Latest       time.Duration
	P50          time.Duration
	P95          time.Duration
}

// TunnelSnapshot freezes a tunnel's counters when EndedAt becomes nonzero.
type TunnelSnapshot struct {
	ID         uint64
	Generation uint64
	Target     string
	Direction  Direction
	StartedAt  time.Time
	EndedAt    time.Time
	Bytes      Bytes
	Error      string
}

// SessionSnapshot contains only the latest generation's measurements.
type SessionSnapshot struct {
	Generation uint64
	Active     bool
	StartedAt  time.Time
	EndedAt    time.Time
	Bytes      Bytes
	RTT        RTT
}

// Event retains at most 512 bytes of each text field.
type Event struct {
	At         time.Time
	Generation uint64
	TunnelID   uint64
	Kind       string
	Message    string
	Error      string
}

// Snapshot owns all its slices and pointers; editing them cannot affect Tracker
// or a later snapshot. Totals include ended generations; Rate is current traffic.
type Snapshot struct {
	At                time.Time
	Session           SessionSnapshot
	Totals            Bytes
	ActiveConnections int
	Rate              Rate
	Active            []TunnelSnapshot
	History           []TunnelSnapshot
	Events            []Event
	LastError         *Event
}

// Tracker follows one logical peer through successive session generations.
// All methods and tunnel handles are safe for concurrent use.
type Tracker struct {
	mu        sync.Mutex
	now       func() time.Time
	options   Options
	started   time.Time
	session   SessionSnapshot
	totals    Bytes
	nextID    uint64
	active    map[uint64]*Tunnel
	history   ring[TunnelSnapshot]
	rtts      ring[time.Duration]
	events    ring[Event]
	lastError *Event
	rates     [rateBuckets]rateBucket
}

type rateBucket struct {
	slot  int64
	bytes Bytes
}

// New allocates only bounded storage. The zero Tracker is not usable.
func New(o Options) *Tracker {
	o.ActiveTunnels = bounded(o.ActiveTunnels, 256, MaxActiveTunnels)
	o.TunnelHistory = bounded(o.TunnelHistory, 128, MaxHistory)
	o.RTTSamples = bounded(o.RTTSamples, 128, MaxRTTSamples)
	o.EventHistory = bounded(o.EventHistory, 128, MaxEvents)
	if o.RateWindow <= 0 {
		o.RateWindow = time.Second
	}
	o.RateWindow = min(max(o.RateWindow, 100*time.Millisecond), time.Minute)
	o.RateWindow -= o.RateWindow % rateBuckets
	t := &Tracker{now: time.Now, options: o, started: time.Now(), active: make(map[uint64]*Tunnel), history: newRing[TunnelSnapshot](o.TunnelHistory), rtts: newRing[time.Duration](o.RTTSamples), events: newRing[Event](o.EventHistory)}
	for i := range t.rates {
		t.rates[i].slot = -1
	}
	return t
}

// BeginSession ends the previous generation and its active tunnels. Late calls
// on old handles are ignored and cannot contaminate a reconnected session.
func (t *Tracker) BeginSession() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.endSession(now, "session replaced")
	t.session = SessionSnapshot{Generation: t.session.Generation + 1, Active: true, StartedAt: now}
	t.rtts.clear()
	t.rates = [rateBuckets]rateBucket{}
	for i := range t.rates {
		t.rates[i].slot = -1
	}
	t.started = now
	t.event(now, 0, "session-start", "", "")
	return t.session.Generation
}

// EndSession is idempotent, including cleanup from an older generation.
func (t *Tracker) EndSession(generation uint64, err error) {
	reason := errorText(err)
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.session.Active || generation != t.session.Generation {
		return
	}
	t.endSession(t.now(), reason)
}

func (t *Tracker) endSession(now time.Time, reason string) {
	if !t.session.Active {
		return
	}
	for _, tunnel := range t.active {
		t.endTunnel(tunnel, now, reason)
	}
	t.session.Active = false
	t.session.EndedAt = now
	t.event(now, 0, "session-end", "", reason)
}

// BeginTunnel's direction is the opener; AddIn/AddOut always use local byte
// direction. Generation must be the active value returned by BeginSession.
func (t *Tracker) BeginTunnel(generation uint64, target string, direction Direction) (*Tunnel, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.session.Active || generation != t.session.Generation {
		return nil, ErrGeneration
	}
	if direction != Inbound && direction != Outbound {
		return nil, fmt.Errorf("observe: invalid tunnel direction %q", direction)
	}
	if len(t.active) >= t.options.ActiveTunnels {
		return nil, ErrLimit
	}
	t.nextID++
	now := t.now()
	h := &Tunnel{tracker: t, snapshot: TunnelSnapshot{ID: t.nextID, Generation: generation, Target: text(target), Direction: direction, StartedAt: now}}
	t.active[h.snapshot.ID] = h
	t.event(now, h.snapshot.ID, "tunnel-start", h.snapshot.Target, "")
	return h, nil
}

// Tunnel counts until End or until its session ends, whichever happens first.
type Tunnel struct {
	tracker  *Tracker
	snapshot TunnelSnapshot
	ended    bool
}

// AddIn counts payload received from the remote peer.
func (h *Tunnel) AddIn(n uint64) { h.add(Bytes{In: n}) }

// AddOut counts payload sent to the remote peer.
func (h *Tunnel) AddOut(n uint64) { h.add(Bytes{Out: n}) }

func (h *Tunnel) add(b Bytes) {
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	if h.ended {
		return
	}
	add(&h.snapshot.Bytes, b)
	add(&t.totals, b)
	add(&t.session.Bytes, b)
	slot := int64(t.now().Sub(t.started) / (t.options.RateWindow / rateBuckets))
	bucket := &t.rates[slot%rateBuckets]
	if bucket.slot != slot {
		*bucket = rateBucket{slot: slot}
	}
	add(&bucket.bytes, b)
}

// End freezes the final counters; repeated End calls cannot duplicate history.
func (h *Tunnel) End(err error) {
	reason := errorText(err)
	t := h.tracker
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endTunnel(h, t.now(), reason)
}

func (t *Tracker) endTunnel(h *Tunnel, now time.Time, reason string) {
	if h.ended {
		return
	}
	h.ended = true
	h.snapshot.EndedAt = now
	h.snapshot.Error = reason
	delete(t.active, h.snapshot.ID)
	t.history.push(h.snapshot)
	t.event(now, h.snapshot.ID, "tunnel-end", "", reason)
}

// AddRTT accepts local-to-peer round-trip measurements for the active generation.
// Negative values and stale samples are ignored. Zero is a valid measured value.
func (t *Tracker) AddRTT(generation uint64, sample time.Duration) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.session.Active || generation != t.session.Generation || sample < 0 {
		return false
	}
	t.rtts.push(sample)
	t.session.RTT.TotalSamples++
	t.session.RTT.Latest = sample
	return true
}

// RecordEvent retains bounded text, not the caller's error objects or buffers.
// An error also updates LastError until replaced, across session generations.
func (t *Tracker) RecordEvent(generation uint64, kind, message string, err error) {
	reason := errorText(err)
	t.mu.Lock()
	defer t.mu.Unlock()
	if generation != t.session.Generation {
		return
	}
	t.event(t.now(), 0, kind, message, reason)
}

func (t *Tracker) event(now time.Time, id uint64, kind, message, reason string) {
	e := Event{At: now, Generation: t.session.Generation, TunnelID: id, Kind: text(kind), Message: text(message), Error: reason}
	t.events.push(e)
	if reason != "" {
		t.lastError = &e
	}
}

// Snapshot copies bounded collections while holding one consistent view.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	s := Snapshot{At: now, Session: t.session, Totals: t.totals, ActiveConnections: len(t.active), Active: make([]TunnelSnapshot, 0, len(t.active)), History: t.history.copy(), Events: t.events.copy()}
	if t.lastError != nil {
		e := *t.lastError
		s.LastError = &e
	}
	for _, h := range t.active {
		s.Active = append(s.Active, h.snapshot)
	}
	slices.SortFunc(s.Active, func(a, b TunnelSnapshot) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	s.Session.RTT.Direction = "local-to-peer-round-trip"
	values := t.rtts.copy()
	s.Session.RTT.Samples = len(values)
	if len(values) > 0 {
		slices.Sort(values)
		s.Session.RTT.P50 = values[(len(values)-1)/2]
		s.Session.RTT.P95 = values[(95*len(values)+99)/100-1]
	}
	s.Rate.Window = min(max(now.Sub(t.started), 0), t.options.RateWindow)
	slot := int64(now.Sub(t.started) / (t.options.RateWindow / rateBuckets))
	var b Bytes
	for _, bucket := range t.rates {
		if bucket.slot >= 0 && bucket.slot <= slot && bucket.slot > slot-rateBuckets {
			add(&b, bucket.bytes)
		}
	}
	if s.Rate.Window > 0 {
		s.Rate.InBytesPerSecond = float64(b.In) / s.Rate.Window.Seconds()
		s.Rate.OutBytesPerSecond = float64(b.Out) / s.Rate.Window.Seconds()
	}
	return s
}

func bounded(n, fallback, limit int) int {
	if n <= 0 {
		return fallback
	}
	return min(n, limit)
}

func add(dst *Bytes, src Bytes) { dst.In += src.In; dst.Out += src.Out }

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return text(err.Error())
}

func text(s string) string {
	if len(s) > maxTextBytes {
		s = s[:maxTextBytes]
		for !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	return strings.Clone(s)
}

type ring[T any] struct {
	values []T
	next   int
	count  int
}

func newRing[T any](limit int) ring[T] { return ring[T]{values: make([]T, limit)} }
func (r *ring[T]) push(value T) {
	r.values[r.next] = value
	r.next = (r.next + 1) % len(r.values)
	r.count = min(r.count+1, len(r.values))
}
func (r *ring[T]) copy() []T {
	values := make([]T, r.count)
	for i := range values {
		values[i] = r.values[(r.next-r.count+i+len(r.values))%len(r.values)]
	}
	return values
}
func (r *ring[T]) clear() { clear(r.values); r.next = 0; r.count = 0 }
