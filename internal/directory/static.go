// Package directory separates peer discovery from connection establishment.
package directory

import (
	"context"
	"errors"
	"fmt"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

// ErrNotFound means the directory has no entry for the requested identity.
var ErrNotFound = errors.New("directory: peer not found")

// Directory returns candidate paths; resolution does not imply availability.
type Directory interface {
	Resolve(ctx context.Context, peer identity.Fingerprint) (transport.PeerInfo, error)
	Announce(ctx context.Context, self transport.PeerInfo) error
}

// Static is immutable after construction and safe for concurrent readers.
type Static struct {
	entries map[identity.Fingerprint]transport.PeerInfo
}

// NewStatic snapshots entries; the last entry for an identity wins.
func NewStatic(entries ...transport.PeerInfo) *Static {
	s := &Static{entries: make(map[identity.Fingerprint]transport.PeerInfo, len(entries))}
	for _, entry := range entries {
		entry.Endpoints = append([]transport.Endpoint(nil), entry.Endpoints...)
		s.entries[entry.ID] = entry
	}
	return s
}

func (s *Static) Resolve(ctx context.Context, peer identity.Fingerprint) (transport.PeerInfo, error) {
	if err := ctx.Err(); err != nil {
		return transport.PeerInfo{}, err
	}
	entry, ok := s.entries[peer]
	if !ok {
		return transport.PeerInfo{}, fmt.Errorf("directory: resolve %s: %w", peer, ErrNotFound)
	}
	entry.Endpoints = append([]transport.Endpoint(nil), entry.Endpoints...)
	return entry, nil
}

// Announce leaves the static snapshot unchanged.
func (s *Static) Announce(ctx context.Context, self transport.PeerInfo) error {
	return ctx.Err()
}

var _ Directory = (*Static)(nil)
