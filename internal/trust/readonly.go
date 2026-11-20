package trust

import (
	"context"
	"fmt"
	"os"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

// ReadPeer obtains saved endpoints, including dial credentials, from the atomic
// trust snapshot without creating a store, lock directory or identity.
func ReadPeer(ctx context.Context, dir string, id identity.Fingerprint) (transport.PeerInfo, error) {
	if err := ctx.Err(); err != nil {
		return transport.PeerInfo{}, err
	}
	if dir == "" || id.IsZero() {
		return transport.PeerInfo{}, ErrNotFound
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return transport.PeerInfo{}, fmt.Errorf("trust: open read-only directory: %w", err)
	}
	defer root.Close()
	info, err := root.Lstat("trust.json")
	if err != nil {
		return transport.PeerInfo{}, fmt.Errorf("trust: inspect snapshot: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return transport.PeerInfo{}, ErrCorrupt
	}
	// Windows Lstat loads file IDs lazily; load it before holding the reader.
	if !os.SameFile(info, info) {
		return transport.PeerInfo{}, ErrCorrupt
	}
	f, err := openPublicSnapshot(root)
	if err != nil {
		return transport.PeerInfo{}, fmt.Errorf("trust: read snapshot: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return transport.PeerInfo{}, fmt.Errorf("trust: stat snapshot: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Size() > 1<<20 || !os.SameFile(info, opened) {
		return transport.PeerInfo{}, ErrCorrupt
	}
	st, err := decodeState(f)
	if err != nil {
		return transport.PeerInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return transport.PeerInfo{}, err
	}
	entries, ok := st.Peers[id.String()]
	if !ok {
		return transport.PeerInfo{}, ErrNotFound
	}
	peer := transport.PeerInfo{ID: id}
	for _, e := range entries {
		endpoint, err := e.endpoint()
		if err != nil {
			return transport.PeerInfo{}, err
		}
		peer.Endpoints = append(peer.Endpoints, endpoint)
	}
	return peer, nil
}
