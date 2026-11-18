package trust

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

func TestReadPeerDoesNotCreateOrMutateState(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	if _, err := ReadPeer(t.Context(), dir, identity.Fingerprint{1}); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("created directory: %v", err)
	}
	s, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	peer := transport.PeerInfo{ID: identity.Fingerprint{1}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "127.0.0.1:47000", Hub: identity.Fingerprint{2}}}}
	if err := s.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	token, err := s.Issue(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	filesBefore, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadPeer(t.Context(), dir, peer.ID)
	if err != nil || !reflect.DeepEqual(got, peer) {
		t.Fatalf("read public peer: %+v %v", got, err)
	}
	got.Endpoints[0].Addr = "mutated"
	next, err := ReadPeer(t.Context(), dir, peer.ID)
	if err != nil || !reflect.DeepEqual(next, peer) {
		t.Fatalf("borrowed endpoints: %+v %v", next, err)
	}
	after, err := os.ReadFile(s.path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read changed trust state")
	}
	filesAfter, err := os.ReadDir(dir)
	if err != nil || len(filesBefore) != len(filesAfter) {
		t.Fatal("read created a file or lock")
	}
	if accepted, err := s.Consume(t.Context(), identity.Fingerprint{3}, token); err != nil || !accepted {
		t.Fatalf("read consumed invite: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ReadPeer(ctx, dir, peer.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %v", err)
	}
}

func TestReadPeerRejectsMalformedSnapshot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trust.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"trusted":{},"invites":{},"peers":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPeer(t.Context(), dir, identity.Fingerprint{1}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown peer: %v", err)
	}
	for _, data := range [][]byte{[]byte(`{}`), []byte(`{"version":100}`), bytes.Repeat([]byte(" "), (1<<20)+1)} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPeer(t.Context(), dir, identity.Fingerprint{1}); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("corrupt accepted: %v", err)
		}
	}
}
