package trust

import (
	"bytes"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

const versionOneGolden = `{"version":1,"trusted":{},"invites":{},"peers":{"AEAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA":[{"kind":"hub","addr":"localhost:47000","hub":"AIAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA-AAAA"}]}}`

func TestPeerTicketVersionSelection(t *testing.T) {
	s := openStore(t)
	if err := os.WriteFile(s.path, []byte(versionOneGolden), 0o600); err != nil {
		t.Fatal(err)
	}
	id := identity.Fingerprint{1}
	peer := transport.PeerInfo{ID: id, Endpoints: []transport.Endpoint{
		{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{2}},
	}}
	got, err := s.Peer(t.Context(), id)
	if err != nil || !reflect.DeepEqual(got, peer) {
		t.Fatalf("version 1 read did not match: %v", err)
	}
	assertSnapshot := func(want string) {
		t.Helper()
		raw, err := os.ReadFile(s.path)
		if err != nil || !bytes.Equal(raw, []byte(want)) {
			t.Fatalf("unexpected snapshot bytes: %v", err)
		}
	}
	if err := s.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(versionOneGolden)

	peer.Endpoints[0].Ticket = "\xff"
	if err := s.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	versionTwo := strings.Replace(versionOneGolden, `"version":1`, `"version":2`, 1)
	versionTwo = strings.TrimSuffix(versionTwo, `"}]}}`) + `","ticket":"_w"}]}}`
	assertSnapshot(versionTwo)
	got, err = s.Peer(t.Context(), id)
	if err != nil || !reflect.DeepEqual(got, peer) {
		t.Fatalf("version 2 store read did not match: %v", err)
	}
	got, err = ReadPeer(t.Context(), filepath.Dir(s.path), id)
	if err != nil || !reflect.DeepEqual(got, peer) {
		t.Fatalf("version 2 snapshot read did not match: %v", err)
	}

	peer.Endpoints[0].Ticket = ""
	if err := s.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	assertSnapshot(versionOneGolden)
}

func TestDecodeTicketSchema(t *testing.T) {
	for _, tc := range []struct {
		name, version, field string
		want                 string
		corrupt              bool
	}{
		{name: "omitted version 2", version: "2"},
		{name: "one byte", version: "2", field: `,"ticket":"_w"`, want: "\xff"},
		{name: "1024 bytes", version: "2", field: ticketField(1024), want: strings.Repeat("\xff", 1024)},
		{name: "1025 bytes", version: "2", field: ticketField(1025), corrupt: true},
		{name: "present version 1", version: "1", field: `,"ticket":"_w"`, corrupt: true},
		{name: "null version 1", version: "1", field: `,"ticket":null`, corrupt: true},
		{name: "empty", version: "2", field: `,"ticket":""`, corrupt: true},
		{name: "null", version: "2", field: `,"ticket":null`, corrupt: true},
		{name: "object", version: "2", field: `,"ticket":{"credential-marker":true}`, corrupt: true},
		{name: "invalid alphabet", version: "2", field: `,"ticket":"credential-marker+/"`, corrupt: true},
		{name: "padding", version: "2", field: `,"ticket":"_w=="`, corrupt: true},
		{name: "line break", version: "2", field: `,"ticket":"_\nw"`, corrupt: true},
		{name: "nonzero pad bits", version: "2", field: `,"ticket":"_x"`, corrupt: true},
		{name: "duplicate", version: "2", field: `,"ticket":"_w","ticket":"credential-marker"`, corrupt: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := strings.Replace(versionOneGolden, `"version":1`, `"version":`+tc.version, 1)
			raw = strings.TrimSuffix(raw, `}]}}`) + tc.field + `}]}}`
			st, err := decodeState(strings.NewReader(raw))
			if tc.corrupt {
				if !errors.Is(err, ErrCorrupt) || strings.Contains(err.Error(), "credential-marker") {
					t.Fatal("invalid snapshot did not return a redacted corruption error")
				}
				return
			}
			if err != nil || string(st.Peers[identity.Fingerprint{1}.String()][0].Ticket) != tc.want {
				t.Fatalf("decoded ticket did not match: %v", err)
			}
		})
	}
	_, err := decodeState(strings.NewReader(`{"future":null,"version":3}`))
	if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("future schema: %v", err)
	}
}

func ticketField(size int) string {
	return `,"ticket":"` + base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{0xff}, size)) + `"`
}

func TestUpdateEndpointTicketConcurrentStores(t *testing.T) {
	s := openStore(t)
	other, err := Open(t.Context(), filepath.Dir(s.path))
	if err != nil {
		t.Fatal(err)
	}
	peer := transport.PeerInfo{ID: identity.Fingerprint{1}, Endpoints: []transport.Endpoint{
		{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{2}, Ticket: "old"},
		{Kind: "hub", Addr: "localhost:47001", Hub: identity.Fingerprint{2}},
		{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{3}, Ticket: "untouched"},
	}}
	if err := s.SavePeer(t.Context(), peer); err != nil {
		t.Fatal(err)
	}
	peer.Endpoints[0].Ticket = strings.Repeat("\xff", 1024)
	peer.Endpoints[1].Ticket = "new"
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, store := range []*Store{s, other} {
		wg.Go(func() {
			<-start
			e := peer.Endpoints[i]
			if err := store.UpdateEndpointTicket(t.Context(), peer.ID, e.Hub, e.Addr, e.Ticket); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Go(func() {
		<-start
		if err := s.Remember(t.Context(), peer.ID); err != nil {
			t.Error(err)
		}
	})
	close(start)
	wg.Wait()
	if trusted, err := other.Trusted(t.Context(), peer.ID); err != nil || !trusted {
		t.Fatalf("concurrent trust update lost: %v", err)
	}
	first := peer.Endpoints[0]
	if err := s.UpdateEndpointTicket(t.Context(), identity.Fingerprint{4}, first.Hub, first.Addr, "new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing peer: %v", err)
	}
	if err := s.UpdateEndpointTicket(t.Context(), peer.ID, first.Hub, "localhost:47002", "new"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing endpoint: %v", err)
	}
	oversize := strings.Repeat("x", 1025)
	for _, ticket := range []string{"", oversize} {
		if err := s.UpdateEndpointTicket(t.Context(), peer.ID, first.Hub, first.Addr, ticket); !errors.Is(err, errInvalidTicket) {
			t.Fatal("renewal accepted an invalid ticket")
		}
	}
	got, err := other.Peer(t.Context(), peer.ID)
	if err != nil || !reflect.DeepEqual(got, peer) {
		t.Fatalf("renewed endpoints did not match: %v", err)
	}
	peer.Endpoints[0].Ticket = oversize
	if err := s.SavePeer(t.Context(), peer); !errors.Is(err, errInvalidTicket) {
		t.Fatal("SavePeer accepted an oversized ticket")
	}
}
