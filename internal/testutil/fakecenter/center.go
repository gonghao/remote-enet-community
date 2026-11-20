// Package fakecenter provides an in-memory TLS fixture exclusively for tests.
// It is not a reference center implementation. Centers must still run C1, C2,
// C9 and C10 concurrently against their real database.
package fakecenter

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
)

type object map[string]any

var encoding = base64.RawURLEncoding.Strict()

func randomID() string    { var b [16]byte; rand.Read(b[:]); return encoding.EncodeToString(b[:]) }
func raw(s string) []byte { b, _ := encoding.DecodeString(s); return b }
func validBytes(s string, n int) bool {
	b, err := encoding.DecodeString(s)
	return err == nil && len(b) == n && encoding.EncodeToString(b) == s
}

// Options defaults to one entry, the wall clock, and 60 tickets per role per minute.
type Options struct {
	CenterID     string
	Entries      int
	Now          func() time.Time
	TicketBudget int
	Limits       Limits
}

// Fault applies once to the next matching request. Delay precedes execution,
// DropResponse aborts the transport after commit, and TimeOffset shifts server_time.
type Fault struct {
	Code              string
	RetryAfter        int
	Delay, TimeOffset time.Duration
	DropResponse      bool
}
type faultKey struct {
	entry int
	path  string
}

// Center serializes transactions from all entries on a single mutex.
type Center struct {
	opts     Options
	key      ed25519.PrivateKey
	urls     []string
	client   *http.Client
	mu       sync.Mutex
	offset   time.Duration
	changed  chan struct{}
	accounts map[string]*account
	devices  map[string]*device
	nodes    map[string]*node
	sessions map[string]*session
	codes    map[string]*session
	records  map[scope]record
	nonces   map[string]int64
	faults   map[faultKey][]Fault
	calls    map[faultKey]int
	counters map[counterKey]counter
}

// New installs cleanup for every loopback listener.
func New(t testing.TB, o Options) *Center {
	t.Helper()
	if o.CenterID == "" {
		o.CenterID = "test-center"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.TicketBudget == 0 {
		o.TicketBudget = 60
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	c := &Center{opts: o, counters: map[counterKey]counter{}, key: key, changed: make(chan struct{}), accounts: map[string]*account{}, devices: map[string]*device{}, nodes: map[string]*node{}, sessions: map[string]*session{}, codes: map[string]*session{}, records: map[scope]record{}, nonces: map[string]int64{}, faults: map[faultKey][]Fault{}, calls: map[faultKey]int{}}
	for i := range max(1, o.Entries) {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.serve(i, w, r) }))
		t.Cleanup(srv.Close)
		c.urls = append(c.urls, srv.URL)
		c.client = srv.Client()
	}
	return c
}

// Entries returns URLs in fallback order.
func (c *Center) Entries() []string { return slices.Clone(c.urls) }

// HTTPClient trusts the httptest certificate shared by the entries.
func (c *Center) HTTPClient() *http.Client { return c.client }

// TicketKey supplies the trust anchor for a test Hub.
func (c *Center) TicketKey() ed25519.PublicKey { return c.key.Public().(ed25519.PublicKey) }
func (c *Center) now() int64                   { return c.opts.Now().Add(c.offset).Unix() }
func (c *Center) notify()                      { close(c.changed); c.changed = make(chan struct{}) }

// Advance moves the transaction clock and wakes pending reads.
func (c *Center) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
	c.sweep(c.now())
	c.notify()
}

// Inject queues a fault for a zero-based entry and exact URL path.
func (c *Center) Inject(entry int, path string, f Fault) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.Code != "" && statuses[f.Code] == 0 {
		panic("unrecognized fault error code")
	}
	k := faultKey{entry, path}
	c.faults[k] = append(c.faults[k], f)
}

// Calls counts attempts including injected failures.
func (c *Center) Calls(entry int, path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls[faultKey{entry, path}]
}

type account struct {
	id, label, status string
	until             int64
	quota             int
}
type device struct {
	account     *account
	fp, label   string
	login, used int64
	revoked     bool
}

func (d *device) expires() int64 { return min(d.login+180*86400, d.used+30*86400) }

// CreateAccount defaults to a single slot and 24 hours of validity.
func (c *Center) CreateAccount(label string, quota int, until time.Time) (id, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if until.IsZero() {
		until = time.Unix(c.now()+86400, 0)
	}
	if quota == 0 {
		quota = 1
	}
	code = c.accountCode()
	id = randomID()
	c.accounts[code] = &account{id: id, label: label, status: "active", until: until.Unix(), quota: quota}
	return
}
func (c *Center) accountCode() string {
	for {
		var b [8]byte
		rand.Read(b[:])
		for i, v := range b {
			b[i] = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"[v%32]
		}
		code := string(b[:])
		if c.accounts[code] == nil {
			return code
		}
	}
}

// ResetAccountCode revokes all enrolled devices and returns a fresh login code.
func (c *Center) ResetAccountCode(code string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweep(now)
	a := c.accounts[code]
	next := c.accountCode()
	delete(c.accounts, code)
	c.accounts[next] = a
	for _, d := range c.devices {
		if d.account == a {
			d.revoked = true
		}
	}
	for _, s := range c.sessions {
		if s.account == a {
			c.end(s, "device_revoked", now)
		}
	}
	c.notify()
	return next
}

// SetAccount applies old deadlines before changing the fixture's policy.
func (c *Center) SetAccount(code, status string, quota int, until time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweep(now)
	a := c.accounts[code]
	a.status = status
	a.quota = quota
	a.until = until.Unix()
	c.sweep(now)
	c.notify()
}

// NodeConfig describes the pre-checked address and identity supplied by a test.
type NodeConfig struct {
	FP             identity.Fingerprint
	Addr, Region   string
	Port, Capacity int
}
type node struct {
	NodeConfig
	id, state string
	pub       ed25519.PublicKey
	gen       uint32
	seq       uint64
	last      int64
	kids      []string
	load      int
}

// RegisterNode reserves an ID. Allocation requires signed activation.
func (c *Center) RegisterNode(cfg NodeConfig) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cfg.Capacity == 0 {
		cfg.Capacity = 1
	}
	id := randomID()
	c.nodes[id] = &node{NodeConfig: cfg, id: id, state: "active"}
	return id
}

// SetNodeState accepts active, draining or disabled.
func (c *Center) SetNodeState(id, state string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweep(now)
	c.nodes[id].state = state
	c.sweep(now)
	c.notify()
}

// RevokeSession simulates an administrative revocation.
func (c *Center) RevokeSession(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	c.sweep(now)
	c.end(c.sessions[id], "admin_revoked", now)
	c.notify()
}

// SessionState exposes accounting values independently of the role-filtered API.
type SessionState struct {
	State, Reason           string
	Released                bool
	CarryBound, OldGenBound int64
	Usage                   Usage
	Tickets                 int
}

// Snapshot includes ended sessions and copies all mutable data.
func (c *Center) Snapshot() map[string]SessionState {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweep(c.now())
	out := map[string]SessionState{}
	for id, s := range c.sessions {
		v := SessionState{State: s.state, Reason: s.reason, Released: s.released, CarryBound: s.bound, OldGenBound: s.old, Tickets: s.issued}
		for _, u := range s.usage {
			v.Usage.RX += u.RX
			v.Usage.TX += u.TX
			v.Usage.Paired += u.Paired
		}
		out[id] = v
	}
	return out
}
func sorted[T any](m map[string]T) []string { return slices.Sorted(maps.Keys(m)) }
