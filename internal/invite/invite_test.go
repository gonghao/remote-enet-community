package invite

import (
	"encoding/binary"
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

func TestTicketVector(t *testing.T) {
	// The first endpoint has no ticket, while the second ends with opaque bytes.
	raw, err := hex.DecodeString("02" + strings.Repeat("00", 48) + "00000000010203040002" +
		"0003687562000178" + strings.Repeat("00", 32) + "0000" +
		"000368756200017901" + strings.Repeat("00", 31) + "0002ff00")
	if err != nil {
		t.Fatal(err)
	}
	want := Invite{Version: 2, ExpiresAt: time.Unix(0x01020304, 0).UTC()}
	want.Peer.Endpoints = []transport.Endpoint{
		{Kind: "hub", Addr: "x"},
		{Kind: "hub", Addr: "y", Hub: [32]byte{1}, Ticket: "\xff\x00"},
	}
	code, err := want.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if code != "renet1:"+encoding.EncodeToString(raw) {
		t.Fatal("version 2 vector mismatch")
	}
	now := time.Unix(0, 0)
	got, err := DecodeAt(code, now)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("version 2 vector did not round trip", err)
	}
	for n := range len(raw) {
		if _, err := DecodeAt("renet1:"+encoding.EncodeToString(raw[:n]), now); err == nil {
			t.Errorf("accepted truncated record at %d bytes", n)
		}
	}
	if _, err := DecodeAt("renet1:"+encoding.EncodeToString(append(raw, 0)), now); err == nil {
		t.Fatal("accepted trailing byte")
	}
}

func TestTicketLengths(t *testing.T) {
	now := time.Now()
	i := Invite{Version: 1, ExpiresAt: now.Add(time.Hour).Truncate(time.Second).UTC()}
	i.Peer.Endpoints = []transport.Endpoint{{Kind: "hub", Addr: "example.test:443"}}
	v1, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	headerAndEndpoint, err := encoding.DecodeString(v1[7:])
	if err != nil {
		t.Fatal(err)
	}
	headerAndEndpoint[0] = 2
	i.Version = 2
	for _, size := range []int{0, 1, 1024, 1025} {
		ticket := strings.Repeat("\xff", size)
		i.Peer.Endpoints[0].Ticket = ticket
		code, err := i.Encode()
		valid := size > 0 && size <= 1024
		if (err == nil) != valid {
			t.Fatalf("Encode with ticket length %d: %v", size, err)
		}
		if valid {
			got, err := ParseInput(code)
			if err != nil || got.Kind != Raw || !reflect.DeepEqual(got.Invite, i) {
				t.Fatalf("ticket length %d did not round trip: %v", size, err)
			}
			continue
		}
		// Construct a complete version 2 record the encoder would refuse.
		raw := binary.BigEndian.AppendUint16(headerAndEndpoint, uint16(size))
		raw = append(raw, ticket...)
		if _, err := DecodeAt("renet1:"+encoding.EncodeToString(raw), now); err == nil {
			t.Errorf("Decode accepted ticket length %d", size)
		}
	}
}

func TestTicketVersionMismatch(t *testing.T) {
	i := Invite{Version: 1, Peer: transport.PeerInfo{Endpoints: []transport.Endpoint{{Ticket: "x"}}}}
	if _, err := i.Encode(); err == nil {
		t.Fatal("version 1 accepted a ticket")
	}
}

func TestLegacyVersionGate(t *testing.T) {
	legacyDecode := func(code string) error {
		raw, err := encoding.DecodeString(code[7:])
		if err != nil {
			return err
		}
		// The old decoder checks the record version before reading the header.
		if raw[0] != 1 {
			return ErrInvalidInvite
		}
		_, err = DecodeAt(code, time.Unix(0, 0))
		return err
	}
	i := Invite{Version: 1, ExpiresAt: time.Unix(100, 0)}
	code, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDecode(code); err != nil {
		t.Fatal("legacy decoder rejected version 1", err)
	}
	i.Version = 2
	i.Peer.Endpoints = []transport.Endpoint{{Ticket: "x"}}
	code, err = i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := legacyDecode(code); err != ErrInvalidInvite {
		t.Fatal("legacy decoder did not reject version 2", err)
	}
}

func TestTicketRecordSize(t *testing.T) {
	i := Invite{Version: 2, ExpiresAt: time.Unix(100, 0)}
	full := transport.Endpoint{Kind: "hub", Addr: "x:1", Ticket: strings.Repeat("t", 1024)}
	for range 61 {
		i.Peer.Endpoints = append(i.Peer.Endpoints, full)
	}
	// 59-byte header, 61 full 1068-byte endpoints, then a 329-byte endpoint.
	i.Peer.Endpoints = append(i.Peer.Endpoints, transport.Endpoint{Kind: "hub", Addr: "x:1", Ticket: strings.Repeat("t", 285)})
	code, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if len(code) != 7+encoding.EncodedLen(64*1024) {
		t.Fatal("record did not reach the size ceiling")
	}
	if _, err := DecodeAt(code, time.Unix(0, 0)); err != nil {
		t.Fatal(err)
	}
	i.Peer.Endpoints[len(i.Peer.Endpoints)-1].Ticket += "t"
	if _, err := i.Encode(); err == nil {
		t.Fatal("accepted a record over the size ceiling")
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
	for _, version := range []uint8{2, 3} {
		i.Version = version
		if _, err := i.Encode(); err == nil {
			t.Fatalf("encoded empty invitation as version %d", version)
		}
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
