// Package exclusive admits local TCP connections from one process at a time.
package exclusive

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"time"
)

var (
	ErrUnsupported  = errors.New("exclusive: owner resolution unsupported")
	ErrUnknownOwner = errors.New("exclusive: unknown connection owner")
	ErrBusy         = errors.New("exclusive: another process owns the gateway")
)

const resolveTimeout = 500 * time.Millisecond

// Identity includes the process creation FILETIME, not an executable name or a
// parent PID. A reused PID must acquire ownership independently.
type Identity struct {
	PID     uint32
	Created uint64
}

// Resolver receives the accepted socket's endpoints in server order. It must
// identify the actual client socket, support concurrent calls, and stop when
// ctx is canceled.
type Resolver interface {
	Resolve(ctx context.Context, acceptedLocal, acceptedRemote netip.AddrPort) (Identity, error)
}

// NewResolver uses the local Windows IPv4 TCP table. Other platforms return
// ErrUnsupported; no address-based identity fallback is permitted.
func NewResolver() Resolver { return systemResolver{} }

// Snapshot is a value copy. A zero Owner means there are no admitted connections.
type Snapshot struct {
	Enabled bool
	Owner   Identity
	Active  int
}

// Gate shares ownership across listeners. Construct it with NewGate.
type Gate struct {
	resolver Resolver
	mu       sync.Mutex
	owner    Identity
	active   int
}

// NewGate uses the platform resolver when resolver is nil.
func NewGate(resolver Resolver) *Gate {
	if resolver == nil {
		resolver = NewResolver()
	}
	return &Gate{resolver: resolver}
}

// Admit reserves ownership before any tunnel is opened. Resolution has a 500ms
// budget (or ctx's earlier deadline); unknown identities never receive a lease.
func (g *Gate) Admit(ctx context.Context, local, remote netip.AddrPort) (*Lease, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("exclusive: admit: %w", err)
	}
	owner, err := g.resolver.Resolve(ctx, local, remote)
	if err != nil {
		return nil, fmt.Errorf("exclusive: resolve owner: %w: %w", ErrUnknownOwner, err)
	}
	if owner.PID == 0 || owner.Created == 0 {
		return nil, ErrUnknownOwner
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("exclusive: admit: %w", err)
	}
	if g.active != 0 && g.owner != owner {
		return nil, ErrBusy
	}
	g.owner = owner
	g.active++
	return &Lease{gate: g}, nil
}

// Snapshot never exposes the gate's mutable state.
func (g *Gate) Snapshot() Snapshot {
	g.mu.Lock()
	defer g.mu.Unlock()
	return Snapshot{Enabled: true, Owner: g.owner, Active: g.active}
}

// Lease belongs to one accepted connection and must not be copied.
type Lease struct {
	gate *Gate
	once sync.Once
}

// Release is idempotent. The last connection relinquishes ownership.
func (l *Lease) Release() {
	l.once.Do(func() {
		l.gate.mu.Lock()
		defer l.gate.mu.Unlock()
		l.gate.active--
		if l.gate.active == 0 {
			l.gate.owner = Identity{}
		}
	})
}
