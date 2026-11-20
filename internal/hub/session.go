package hub

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// SessionID binds a route to a center session. Zero selects self-hosted routing.
type SessionID [16]byte

// Usage totals span all connections for a session in this process.
type Usage struct {
	Session          SessionID
	Paired           bool
	Conns            int
	RxBytes, TxBytes uint64
	PairedFor        time.Duration
}

type sessionRecord struct {
	id          SessionID
	deadline    time.Time
	stop        func() bool
	routes      map[*waiter]struct{}
	pairs       int
	pairedSince time.Time
	pairedFor   time.Duration
	rx, tx      atomic.Uint64
}

func (r *sessionRecord) usage(now time.Time) Usage {
	d := r.pairedFor
	if r.pairs > 0 {
		d += now.Sub(r.pairedSince)
	}
	return Usage{
		Session: r.id, Paired: r.pairs > 0, Conns: len(r.routes) + r.pairs,
		RxBytes: r.rx.Load(), TxBytes: r.tx.Load(), PairedFor: d,
	}
}

// A relay meter excludes setup bytes. Closing the raw socket before taking the
// write lock unblocks outstanding I/O and includes its counts in final usage.
type relayMeter struct {
	conn    *connection
	session *sessionRecord
	mu      sync.RWMutex
	stopped bool
}

func (c *connection) relay() io.ReadWriter {
	if c.meter != nil {
		return c.meter
	}
	return c
}

func (m *relayMeter) Read(p []byte) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stopped {
		return 0, net.ErrClosed
	}
	n, err := m.conn.Read(p)
	m.session.rx.Add(uint64(n))
	return n, err
}

func (m *relayMeter) Write(p []byte) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.stopped {
		return 0, net.ErrClosed
	}
	n, err := m.conn.Write(p)
	m.session.tx.Add(uint64(n))
	return n, err
}

func (m *relayMeter) stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopped = true
}

// Entries have inclusive deadlines. Reclaim expired insertion-order prefixes:
// an earlier long ticket can delay storage cleanup, never the logical expiry.
type expiryEntry struct {
	key   [16]byte
	until time.Time
}

type expiringValue[V any] struct {
	until time.Time
	value V
}

type expiryCache[V any] struct {
	entries map[[16]byte]expiringValue[V]
	order   []expiryEntry
}

func (c *expiryCache[V]) get(key [16]byte, now time.Time) (V, bool) {
	e, ok := c.entries[key]
	return e.value, ok && !now.After(e.until)
}

func (c *expiryCache[V]) put(key [16]byte, until, now time.Time, value V) {
	for len(c.order) > 0 && now.After(c.order[0].until) {
		old := c.order[0]
		c.order = c.order[1:]
		if c.entries[old.key].until.Equal(old.until) {
			delete(c.entries, old.key)
		}
	}
	if c.entries == nil {
		c.entries = make(map[[16]byte]expiringValue[V])
	}
	c.entries[key] = expiringValue[V]{until, value}
	c.order = append(c.order, expiryEntry{key, until})
}

// SetAdmitting gates new center grants without disconnecting existing routes.
func (s *Server) SetAdmitting(open bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitting = open
}

func (s *Server) commitSessionLocked(w *waiter, c *connection, g Grant, now time.Time) {
	if g.Session == (SessionID{}) {
		return
	}
	r := s.sessions[g.Session]
	if r == nil {
		r = &sessionRecord{id: g.Session, routes: make(map[*waiter]struct{})}
		s.sessions[g.Session] = r
	}
	idle := len(r.routes) == 0
	r.routes[w] = struct{}{}
	if w.dial != nil {
		if r.pairs == 0 {
			r.pairedSince = now
		}
		r.pairs++
	}
	c.meter = &relayMeter{conn: c, session: r}
	s.consumed.put(g.JTI, g.Expires, now, struct{}{})
	// Anchor the ticket's wall-clock timestamp to Now's monotonic reading.
	deadline := now.Add(g.Expires.Sub(now))
	extended := deadline.After(r.deadline)
	if extended {
		r.deadline = deadline
	}
	if idle || extended {
		s.scheduleLocked(r)
	}
}

func (s *Server) detachSessionLocked(w *waiter) {
	if w.grant.Session == (SessionID{}) {
		return
	}
	r := s.sessions[w.grant.Session]
	w.conn.meter.stop()
	if w.dial != nil {
		w.dial.meter.stop()
		r.pairs--
		if r.pairs == 0 {
			r.pairedFor += s.o.Now().Sub(r.pairedSince)
		}
	}
	delete(r.routes, w)
	if len(r.routes) == 0 {
		s.scheduleLocked(r)
	}
}

func (s *Server) scheduleLocked(r *sessionRecord) {
	if r.stop != nil {
		r.stop()
	}
	due := r.deadline
	if len(r.routes) == 0 {
		due = due.Add(s.o.SessionRetention)
	}
	r.stop = s.o.AfterFunc(due.Sub(s.o.Now()), func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.sessions[r.id] != r {
			return
		}
		now := s.o.Now()
		s.expireRoutesLocked(r, now)
		if len(r.routes) == 0 && !now.Before(r.deadline.Add(s.o.SessionRetention)) {
			s.retireLocked(r)
		}
	})
}

func (s *Server) expireRoutesLocked(r *sessionRecord, now time.Time) {
	if r != nil && !now.Before(r.deadline) {
		for w := range r.routes {
			s.removeWaitLocked(w)
		}
	}
}

// Renew replaces D, even when the center shortens the lease.
func (s *Server) Renew(id SessionID, deadline time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.sessions[id]
	if r == nil {
		return false
	}
	now := s.o.Now()
	s.expireRoutesLocked(r, now)
	r.deadline = deadline
	s.expireRoutesLocked(r, now)
	s.scheduleLocked(r)
	return true
}

// Revoke seals even unknown sessions. Repeated revocation returns final totals.
func (s *Server) Revoke(id SessionID) Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	u, ok := s.sealed.get(id, now)
	if !ok {
		u = Usage{Session: id}
	}
	if r := s.sessions[id]; r != nil {
		for w := range r.routes {
			s.removeWaitLocked(w)
		}
		u = r.usage(now)
		s.retireLocked(r)
	}
	s.sealed.put(id, now.Add(s.o.SealTTL), now, u)
	return u
}

// CloseAll disconnects center routes without sealing their sessions.
func (s *Server) CloseAll() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, r := range s.sessions {
		if len(r.routes) > 0 {
			count++
		}
		for w := range r.routes {
			s.removeWaitLocked(w)
		}
	}
	return count
}

// Usage omits disconnected sessions; Revoke can still collect their tail.
func (s *Server) Usage() []Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.o.Now()
	var result []Usage
	for _, r := range s.sessions {
		if len(r.routes) > 0 {
			result = append(result, r.usage(now))
		}
	}
	return result
}

// TrafficTotals includes idle and retired center sessions so heartbeat deltas
// also cover routes that opened and closed between two samples.
func (s *Server) TrafficTotals() (rx, tx uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rx, tx = s.retiredRx, s.retiredTx
	for _, r := range s.sessions {
		rx += r.rx.Load()
		tx += r.tx.Load()
	}
	return rx, tx
}

func (s *Server) retireLocked(r *sessionRecord) {
	r.stop()
	s.retiredRx += r.rx.Load()
	s.retiredTx += r.tx.Load()
	delete(s.sessions, r.id)
}

func (s *Server) stopSessionTimers() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.sessions {
		s.retireLocked(r)
	}
	s.sealed = expiryCache[Usage]{}
	s.consumed = expiryCache[struct{}]{}
}
