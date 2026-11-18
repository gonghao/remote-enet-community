// Package transport defines the path-independent session abstraction used by
// every layer above the wire: an authenticated, multiplexed connection to one
// peer. How the connection is obtained (embedded hub, standalone hub, ...) is
// the job of a Connector/Acceptor implementation registered by kind.
package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"remote-enet/internal/identity"
)

// ErrUnsupported is returned by optional operations an implementation lacks.
var ErrUnsupported = errors.New("transport: operation not supported")

// Stream is one bidirectional byte stream inside a Session.
//
// CloseWrite half-closes the sending direction so TCP forwarding can relay a
// FIN without tearing down the receive side.
type Stream interface {
	io.ReadWriteCloser
	CloseWrite() error
}

// Session is an authenticated, end-to-end encrypted, multiplexed connection to
// exactly one peer. Implementations must be safe for concurrent use.
type Session interface {
	OpenStream(ctx context.Context) (Stream, error)
	AcceptStream(ctx context.Context) (Stream, error)
	// Peer is the verified fingerprint of the remote device.
	Peer() identity.Fingerprint
	Path() PathInfo
	Stats() LinkStats
	// Close tears down the session; reason may be nil and is reported to Done
	// observers via Err.
	Close(reason error) error
	// Done is closed once the session is unusable for any reason.
	Done() <-chan struct{}
	// Err returns the terminal error after Done is closed, nil before.
	Err() error
}

// PathKind names how the bytes reach the peer. UI shows it, nothing else
// depends on it.
type PathKind string

const (
	// PathDirect means the peer accepted our TCP connection itself (the
	// operator's embedded hub).
	PathDirect PathKind = "direct"
	// PathRelay means a standalone hub is pumping bytes between us.
	PathRelay PathKind = "relay"
)

// PathInfo describes the established path for diagnostics and UI.
type PathInfo struct {
	Kind       PathKind
	LocalAddr  string
	RemoteAddr string
	// Via is the hub address when Kind == PathRelay, empty otherwise.
	Via string
}

// LinkStats is a point-in-time snapshot; implementations may return zero RTT
// when they have not measured one yet.
type LinkStats struct {
	RTT      time.Duration
	BytesIn  uint64
	BytesOut uint64
}

// Endpoint is one candidate way to reach a peer.
type Endpoint struct {
	// Kind selects the registered Connector ("hub" for now).
	Kind string
	// Addr is host:port of the listener to dial (hub or embedded hub).
	Addr string
	// Hub is the fingerprint of the hub's own identity, pinned during the
	// outer TLS handshake. For an embedded hub it equals PeerInfo.ID.
	Hub identity.Fingerprint
}

// PeerInfo is everything needed to reach and verify a peer. It comes from an
// invite code today and from a directory service later.
type PeerInfo struct {
	ID        identity.Fingerprint
	Endpoints []Endpoint
}

// Connector knows how to reach a peer through one kind of path.
type Connector interface {
	Dial(ctx context.Context, peer PeerInfo) (Session, error)
}

// Acceptor yields sessions initiated by remote peers.
type Acceptor interface {
	Accept(ctx context.Context) (Session, error)
	Close() error
}

// Config selects and parameterises a registered transport.
type Config struct {
	Kind    string
	Options map[string]string
}

// Factory builds the connector/acceptor pair for one transport kind. Either
// return value may be nil when the configuration only supports one direction.
type Factory func(cfg Config, local *identity.Local) (Connector, Acceptor, error)

var (
	registryMu sync.RWMutex
	registry   = map[string]Factory{}
)

// Register makes a transport available by kind. It panics on duplicate
// registration because that is always a programming error at init time.
func Register(kind string, f Factory) {
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, dup := registry[kind]; dup {
		panic(fmt.Sprintf("transport: kind %q registered twice", kind))
	}
	registry[kind] = f
}

// New instantiates a registered transport.
func New(cfg Config, local *identity.Local) (Connector, Acceptor, error) {
	registryMu.RLock()
	f, ok := registry[cfg.Kind]
	registryMu.RUnlock()
	if !ok {
		return nil, nil, fmt.Errorf("transport: unknown kind %q (registered: %v)", cfg.Kind, Kinds())
	}
	return f(cfg, local)
}

// Kinds lists registered transport kinds, sorted.
func Kinds() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	kinds := make([]string, 0, len(registry))
	for k := range registry {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	return kinds
}
