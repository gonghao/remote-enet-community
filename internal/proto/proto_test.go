package proto

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"reflect"
	"testing"
	"testing/iotest"
)

func TestRoundTrip(t *testing.T) {
	messages := []any{
		Hello{Version, "client", "test", []string{"tcp"}}, PairToken{[16]byte{1}}, PairResult{true, "ok"},
		VehicleState{"eth0", "169.254.0.1", "169.254.0.2", "VIN", "MAC", 0x12, []byte{1, 2}, 99},
		Ping{1, 2}, Pong{1, 2}, ExposeReverse{80, true}, ExposeResult{80, true, "ok"}, Log{"info", "test", map[string]string{"a": "b"}, 123}, OpenTunnel{"tcp", "127.0.0.1:80", "test"},
	}
	var buf bytes.Buffer
	for _, m := range messages {
		if err := Write(&buf, m); err != nil {
			t.Fatal(err)
		}
	}
	reader := iotest.OneByteReader(&buf)
	for _, want := range messages {
		got, err := Read(reader, MaxFrameLen)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("got %#v, want %#v", got, want)
		}
	}
	if _, err := Read(reader, MaxFrameLen); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}
func TestVector(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, Ping{1, 2}); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0, 0, 9, 0xa2, 1, 5, 2, 0xa2, 1, 1, 2, 2}
	if !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("got %x want %x", buf.Bytes(), want)
	}
}
func TestUnknownThenKnown(t *testing.T) {
	var buf bytes.Buffer
	if err := writeEnvelope(&buf, Envelope{Type: 600, Body: []byte{0xa0}}); err != nil {
		t.Fatal(err)
	}
	if err := Write(&buf, Pong{3, 4}); err != nil {
		t.Fatal(err)
	}
	m, err := Read(&buf, MaxFrameLen)
	if err != nil {
		t.Fatal(err)
	}
	if u, ok := m.(Unknown); !ok || u.Type != 600 {
		t.Fatalf("unknown: %#v", m)
	}
	if m, err = Read(&buf, MaxFrameLen); err != nil || m != (Pong{3, 4}) {
		t.Fatalf("next: %#v %v", m, err)
	}
}
func TestMalformed(t *testing.T) {
	for _, raw := range [][]byte{
		{}, {0, 0}, {0, 0, 0, 0}, {0, 0, 0, 50}, {0, 0, 0, 1, 0xff},
		{0, 0, 0, 5, 0xa2, 1, 1, 1, 2}, // Duplicate envelope key.
		{0, 0, 0, 5, 0xa2, 1, 1, 2, 0}, // Invalid Hello body.
	} {
		if _, err := Read(bytes.NewReader(raw), 16); err == nil {
			t.Fatalf("accepted %x", raw)
		}
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], 0xffffffff)
	if _, err := Read(bytes.NewReader(prefix[:]), MaxFrameLen); err == nil {
		t.Fatal("unbounded length")
	}
	if err := Write(io.Discard, struct{}{}); err == nil {
		t.Fatal("unsupported type")
	}
	var nilPing *Ping
	if err := Write(io.Discard, nilPing); err == nil {
		t.Fatal("nil pointer")
	}
}

type shortWriter struct{ bytes.Buffer }

func (w *shortWriter) Write(b []byte) (int, error) { return w.Buffer.Write(b[:1]) }
func TestShortWrites(t *testing.T) {
	w := new(shortWriter)
	p := Ping{8, 9}
	if err := Write(w, &p); err != nil {
		t.Fatal(err)
	}
	got, err := Read(w, MaxFrameLen)
	if err != nil || got != p {
		t.Fatalf("%v %v", got, err)
	}
}
func FuzzRead(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1, 0xa0})
	f.Fuzz(func(t *testing.T, b []byte) { _, _ = Read(bytes.NewReader(b), 4096) })
}

func TestBoundedMalformedCBOR(t *testing.T) {
	frame := func(body []byte) []byte {
		raw := make([]byte, 4+len(body))
		binary.BigEndian.PutUint32(raw, uint32(len(body)))
		copy(raw[4:], body)
		return raw
	}
	for _, body := range [][]byte{
		{0xf6},                            // Null is not an envelope.
		{0xa0},                            // Missing type and body.
		{0xa1, 1, 5},                      // Missing body.
		{0xa2, 1, 5, 2, 0xa2, 1, 1, 1, 2}, // Duplicate body keys.
		append(append([]byte{0xa2, 1, 0x19, 3, 0xe7, 2}, bytes.Repeat([]byte{0x81}, 100)...), 0),
		{0xa2, 1, 0x19, 3, 0xe7, 2, 0xff}, // Malformed unknown body.
	} {
		if _, err := Read(bytes.NewReader(frame(body)), MaxFrameLen); err == nil {
			t.Fatalf("accepted malformed CBOR %x", body)
		}
	}
}

func TestReadPairRequiresCanonicalFixedBody(t *testing.T) {
	point := make([]byte, 32)
	body := append([]byte{0xa1, 1, 0x58, 32}, point...)
	env := func(b []byte) []byte { return append([]byte{0xa2, 1, 12, 2}, b...) }
	for name, payload := range map[string][]byte{
		"envelope extension": append(append([]byte{0xa3}, env(body)[1:]...), 3, 0),
		"noncanonical type":  append([]byte{0xa2, 1, 0x18, 12, 2}, body...),
		"body extension":     env(append(append([]byte{0xa2}, body[1:]...), 2, 0)),
		"duplicate key":      env(append(append([]byte{0xa2}, body[1:]...), 1, 0)),
		"short value":        env(append([]byte{0xa1, 1, 0x58, 31}, point[:31]...)),
		"long value":         env(append([]byte{0xa1, 1, 0x58, 33, 0}, point...)),
		"null":               env([]byte{0xa1, 1, 0xf6}),
		"truncated":          env(body[:len(body)-1]),
	} {
		t.Run(name, func(t *testing.T) {
			frame := binary.BigEndian.AppendUint32(nil, uint32(len(payload)))
			if _, err := ReadPair(bytes.NewReader(append(frame, payload...))); err == nil {
				t.Fatal("accepted malformed frame")
			}
		})
	}
	if _, err := ReadPair(bytes.NewReader([]byte{0, 0, 1, 1})); err == nil || errors.Is(err, io.EOF) {
		t.Fatal("length was not rejected before reading body", err)
	}
	for _, msg := range []any{PakeCommit{}, PairDecision{Accepted: true, Reason: "accepted"}, PakeShareA{}, PakeShareB{}, PakeConfirmA{}, PakeConfirmB{}} {
		var buf bytes.Buffer
		if err := Write(&buf, msg); err != nil {
			t.Fatal(err)
		}
		if got, err := ReadPair(&buf); got != msg || err != nil {
			t.Fatalf("round trip %T: %v", got, err)
		}
	}
}

func TestReadPairRejectsLegacyResult(t *testing.T) {
	var buf bytes.Buffer
	if err := Write(&buf, PairResult{Reason: "expired"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPair(&buf); err == nil {
		t.Fatal("accepted legacy result on pairing stream")
	}
}

func TestHubTicket(t *testing.T) {
	for _, n := range []int{0, 1, 1024, 1025} {
		ticket := HubTicket{Ticket: bytes.Repeat([]byte{23}, n)}
		var frame bytes.Buffer
		err := Write(&frame, &ticket)
		if n == 0 || n > 1024 {
			if err == nil || frame.Len() != 0 {
				t.Errorf("wrote invalid ticket of length %d", n)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ReadPair(bytes.NewReader(frame.Bytes())); err == nil {
			t.Fatal("ReadPair accepted ticket")
		}
		got, err := ReadHubTicket(&frame)
		if err != nil || !bytes.Equal(got.Ticket, ticket.Ticket) {
			t.Fatalf("ticket length %d: %v", n, err)
		}
	}
	if _, err := ReadHubTicket(bytes.NewReader(binary.BigEndian.AppendUint32(nil, 1101))); err == nil || errors.Is(err, io.EOF) {
		t.Fatalf("frame limit: %v", err)
	}
	var other bytes.Buffer
	if err := Write(&other, PairDecision{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadHubTicket(&other); err == nil {
		t.Fatal("accepted wrong message")
	}
}

func TestHubTicketRejectsMalformedBody(t *testing.T) {
	for _, body := range [][]byte{
		{0xa0}, {0xa1, 1, 0x40}, {0xa1, 1, 0xf6}, {0xa1, 1, 0x61, 65},
		{0xa1, 1, 0x58, 1, 0}, {0xa2, 1, 0x41, 0, 2, 0},
		append([]byte{0xa1, 1, 0x59, 4, 1}, make([]byte, 1025)...),
	} {
		var frame bytes.Buffer
		if err := writeEnvelope(&frame, Envelope{Type: 17, Body: body}); err != nil {
			t.Fatal(err)
		}
		if _, err := Read(&frame, MaxFrameLen); err == nil {
			t.Errorf("accepted invalid ticket body (length %d)", len(body))
		}
	}
}
