package directory

import (
	"context"
	"errors"
	"sync"
	"testing"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

func TestStatic(t *testing.T) {
	id := identity.Fingerprint{1}
	entry := transport.PeerInfo{ID: id, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "original"}}}
	s := NewStatic(entry)
	entry.Endpoints[0].Addr = "mutated"
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			got, err := s.Resolve(t.Context(), id)
			if err != nil || got.Endpoints[0].Addr != "original" {
				t.Errorf("resolve: %+v, %v", got, err)
				return
			}
			got.Endpoints[0].Addr = "caller mutation"
		})
	}
	wg.Wait()
	if err := s.Announce(t.Context(), transport.PeerInfo{ID: identity.Fingerprint{2}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(t.Context(), identity.Fingerprint{2}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing peer: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Resolve(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := s.Announce(ctx, entry); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
