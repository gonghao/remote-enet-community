package invite

import (
	"encoding/hex"
	"reflect"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/transport"
)

func TestVector(t *testing.T) {
	// Independently specified bytes: v1, zero ID/token, expiry=0x01020304, one endpoint.
	raw, _ := hex.DecodeString("01" + strings.Repeat("00", 48) + "000000000102030400010003687562000178" + strings.Repeat("00", 32))
	want := Invite{Version: 1, ExpiresAt: time.Unix(0x01020304, 0).UTC(), Peer: transport.PeerInfo{Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "x"}}}}
	code, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if code != "renet1:"+encoding.EncodeToString(raw) {
		t.Fatalf("vector mismatch: %s", code)
	}
	got, err := DecodeAt(strings.ToLower(code), time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestRoundTrip(t *testing.T) {
	i := Invite{Version: 1, ExpiresAt: time.Now().Add(time.Hour).Truncate(time.Second).UTC()}
	i.Peer.ID[3] = 42
	i.Token[5] = 99
	i.Peer.Endpoints = []transport.Endpoint{{Kind: "hub", Addr: "[::1]:443", Hub: i.Peer.ID}, {Kind: "hub", Addr: "example.test:123"}}
	code, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := Decode(code)
	if err != nil || !reflect.DeepEqual(got, i) {
		t.Fatalf("%#v %v", got, err)
	}
}

func TestInvalid(t *testing.T) {
	i := Invite{Version: 1, ExpiresAt: time.Unix(100, 0)}
	code, _ := i.Encode()
	raw, _ := encoding.DecodeString(code[7:])
	cases := []string{"", "renet2:AA", "renet1:!", code + "=", code + "A", "renet1:" + strings.Repeat("A", encoding.EncodedLen(maxSize)+1)}
	for n := range len(raw) {
		cases = append(cases, "renet1:"+encoding.EncodeToString(raw[:n]))
	}
	bad := append([]byte(nil), raw...)
	bad[0] = 2
	cases = append(cases, "renet1:"+encoding.EncodeToString(bad))
	bad = append(append([]byte(nil), raw...), 0)
	cases = append(cases, "renet1:"+encoding.EncodeToString(bad))
	for _, s := range cases {
		if _, err := DecodeAt(s, time.Unix(0, 0)); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	for _, now := range []time.Time{time.Unix(100, 0), time.Unix(101, 0)} {
		if _, err := DecodeAt(code, now); err == nil {
			t.Fatal("accepted expired code")
		}
	}
	i.Version = 2
	if _, err := i.Encode(); err == nil {
		t.Fatal("accepted version")
	}
	i.Version = 1
	i.Peer.Endpoints = []transport.Endpoint{{Kind: strings.Repeat("x", 65536)}}
	if _, err := i.Encode(); err == nil {
		t.Fatal("accepted long string")
	}
}

func FuzzDecode(f *testing.F) {
	i := Invite{Version: 1, ExpiresAt: time.Unix(100, 0)}
	code, _ := i.Encode()
	f.Add(code)
	f.Fuzz(func(t *testing.T, s string) { _, _ = DecodeAt(s, time.Unix(0, 0)) })
}
