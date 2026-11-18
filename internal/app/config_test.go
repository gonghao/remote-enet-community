package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"remote-enet/internal/carside"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func TestRunClientStopsBeforeNetworkingOnPersistentConfigurationFailure(t *testing.T) {
	for _, failure := range []string{"corrupt-store", "invalid-saved-endpoint", "unknown-peer", "invalid-invitation"} {
		t.Run(failure, func(t *testing.T) {
			store, dir := storeForTest(t)
			peer := transport.PeerInfo{ID: identity.Fingerprint{2}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "0.0.0.0:47000", Hub: identity.Fingerprint{2}}}}
			cfg := ClientConfig{StateDir: dir, Peer: peer.ID, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: func(context.Context) ([]carside.NIC, error) {
				t.Error("persistent error reached networking")
				return nil, errors.New("unexpected enumeration")
			}}}
			switch failure {
			case "corrupt-store":
				if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "invalid-saved-endpoint":
				if err := store.SavePeer(t.Context(), peer); err != nil {
					t.Fatal(err)
				}
			case "invalid-invitation":
				cfg.Peer = identity.Fingerprint{}
				cfg.Invite = "invalid-secret"
			}
			r := New()
			err := r.RunClient(t.Context(), cfg)
			if err == nil || r.Snapshot().Phase != PhaseStopped {
				t.Fatalf("persistent failure did not stop: %v %+v", err, r.Snapshot())
			}
			if failure == "corrupt-store" && !errors.Is(err, trust.ErrCorrupt) {
				t.Fatalf("lost corruption error: %v", err)
			}
		})
	}
}
