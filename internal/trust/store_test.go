package trust

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

func openStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestInvitationIsSecretAndConsumedOnce(t *testing.T) {
	s := openStore(t)
	token, err := s.Issue(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, token[:]) || bytes.Contains(raw, []byte(hex.EncodeToString(token[:]))) {
		t.Fatal("state contains plaintext token")
	}
	if !bytes.Contains(raw, []byte(tokenHash(token))) {
		t.Fatal("state does not contain token hash")
	}
	peer := identity.Fingerprint{1}
	for _, v := range [][16]byte{{}, {1}} {
		if accepted, err := s.Consume(t.Context(), peer, v); accepted || err != nil {
			t.Fatalf("invalid token accepted: %v %v", accepted, err)
		}
	}
	second, err := Open(t.Context(), filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int64
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			store := s
			if i%2 == 0 {
				store = second
			}
			ok, err := store.Consume(t.Context(), peer, token)
			if err != nil {
				t.Error(err)
			}
			if ok {
				accepted.Add(1)
			}
		})
	}
	wg.Wait()
	if accepted.Load() != 1 {
		t.Fatalf("successful consumes = %d", accepted.Load())
	}
	reloaded, err := Open(t.Context(), filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	if trusted, err := reloaded.Trusted(t.Context(), peer); !trusted || err != nil {
		t.Fatalf("trust did not persist: %t %v", trusted, err)
	}
	if ok, err := reloaded.Consume(t.Context(), identity.Fingerprint{2}, token); ok || err != nil {
		t.Fatalf("consumed token reusable: %t %v", ok, err)
	}
}

func TestExpiredAndZeroIdentityRejected(t *testing.T) {
	s := openStore(t)
	if _, err := s.Issue(t.Context(), time.Now().Add(-time.Second)); err == nil {
		t.Fatal("past expiry accepted")
	}
	token, err := s.Issue(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Consume(t.Context(), identity.Fingerprint{}, token); ok || err != nil {
		t.Fatalf("zero identity accepted: %t %v", ok, err)
	}
	if err := s.access(t.Context(), func(st *state) (bool, error) {
		st.Invites[tokenHash(token)] = time.Now().Add(-time.Second).UnixNano()
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Consume(t.Context(), identity.Fingerprint{1}, token); ok || err != nil {
		t.Fatalf("expired token accepted: %t %v", ok, err)
	}
}

func TestPeerPersistenceAndFreshReads(t *testing.T) {
	s := openStore(t)
	other, err := Open(t.Context(), filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Fingerprint{1}
	peer := transport.PeerInfo{ID: id, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "localhost:47000", Hub: id}}}
	if err := other.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	peer.Endpoints[0].Addr = "mutated"
	got, err := s.Peer(t.Context(), id)
	if err != nil || got.Endpoints[0].Addr != "localhost:47000" {
		t.Fatalf("saved peer did not match: %v", err)
	}
	got.Endpoints[0].Addr = "caller mutation"
	got, err = s.Peer(t.Context(), id)
	if err != nil || got.Endpoints[0].Addr != "localhost:47000" {
		t.Fatal("caller changed saved peer")
	}
	if _, err := s.Peer(t.Context(), identity.Fingerprint{3}); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := s.SavePeer(t.Context(), transport.PeerInfo{ID: id, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "localhost:0", Hub: id}}}); err == nil {
		t.Fatal("zero endpoint port accepted")
	}
}

func TestCorruptionFailsClosedAndLockIsReleased(t *testing.T) {
	for _, raw := range []string{"", "{", `{"version":3,"trusted":{},"invites":{},"peers":{}}`, `{"version":1,"trusted":{},"invites":{},"peers":{}} {}`, `{"version":1,"trusted":null,"invites":{},"peers":{}}`, `{"version":1,"trusted":{},"invites":{"bad":1},"peers":{}}`} {
		t.Run(raw, func(t *testing.T) {
			s := openStore(t)
			if err := os.WriteFile(s.path, []byte(raw), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(t.Context(), filepath.Dir(s.path)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt state accepted: %v", err)
			}
			if ok, err := s.Trusted(t.Context(), identity.Fingerprint{1}); ok || !errors.Is(err, ErrCorrupt) {
				t.Fatalf("corrupt state used stale trust: %t %v", ok, err)
			}
			if _, err := os.Stat(s.path + ".lock"); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("lock retained after decode failure: %v", err)
			}
		})
	}
}

func TestLockCancellationDoesNotRemoveAnotherOwner(t *testing.T) {
	s := openStore(t)
	if err := os.Mkdir(s.path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Issue(ctx, time.Now().Add(time.Minute)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path + ".lock"); err != nil {
		t.Fatal("another owner's lock was removed")
	}
}

func TestConcurrentIdentityInitialization(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan identity.Fingerprint, 16)
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			store := first
			if i%2 == 0 {
				store = second
			}
			local, err := store.LoadIdentity(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			results <- local.Fingerprint()
		})
	}
	wg.Wait()
	close(results)
	var expected identity.Fingerprint
	count := 0
	for got := range results {
		if expected.IsZero() {
			expected = got
		}
		if got != expected {
			t.Fatalf("different identities: %s != %s", got, expected)
		}
		count++
	}
	if count != 16 {
		t.Fatalf("identity results = %d", count)
	}
}

func TestFailedTransactionDoesNotPersistTrust(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("requires Unix directory permissions")
	}
	s := openStore(t)
	if _, err := s.Issue(t.Context(), time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(s.path)
	err := s.access(t.Context(), func(st *state) (bool, error) {
		st.Trusted[identity.Fingerprint{1}.String()] = true
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		return true, nil
	})
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Failed cleanup could not remove the lock in a read-only directory.
	if err := os.Remove(s.path + ".lock"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err == nil {
		t.Fatal("read-only directory permitted transaction")
	}
	if ok, err := s.Trusted(t.Context(), identity.Fingerprint{1}); ok || err != nil {
		t.Fatalf("failed transaction persisted trust: %t %v", ok, err)
	}
}

func TestRememberConcurrentStoresPreserveVersionOne(t *testing.T) {
	s := openStore(t)
	other, err := Open(t.Context(), filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	token, err := s.Issue(t.Context(), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	done := make(chan error, 2)
	for i, store := range []*Store{s, other} {
		go func() { <-start; done <- store.Remember(t.Context(), identity.Fingerprint{byte(i + 1)}) }()
	}
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.load()
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != 1 || len(st.Trusted) != 2 || st.Invites[tokenHash(token)] == 0 || len(st.Peers) != 0 {
		t.Fatal("invalid version 1 state after remember")
	}
	for _, peer := range []identity.Fingerprint{{1}, {2}} {
		if !st.Trusted[peer.String()] {
			t.Fatal("lost concurrent update")
		}
	}
}
