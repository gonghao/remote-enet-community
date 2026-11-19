// Package proto defines version 1 control frames and data stream headers.
// Frames contain a big-endian uint32 byte length followed by canonical CBOR.
// Envelope key 1 is the stable message number; key 2 is its integer-keyed body.
package proto

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"

	"github.com/fxamacker/cbor/v2"
)

const Version uint16 = 1
const MaxFrameLen = 1 << 20

// PairDirectCapability opts a client into a single numeric redemption.
const PairDirectCapability = "pair-direct/1"

type MsgType uint16

const (
	TypeHello         MsgType = 1
	TypePairToken     MsgType = 2
	TypePairResult    MsgType = 3
	TypeVehicleState  MsgType = 4
	TypePing          MsgType = 5
	TypePong          MsgType = 6
	TypeExposeReverse MsgType = 7
	TypeExposeResult  MsgType = 8
	TypeLog           MsgType = 9
	TypeOpenTunnel    MsgType = 10
	TypePairShareA    MsgType = 11
	TypePairShareB    MsgType = 12
	TypePairConfirmA  MsgType = 13
	TypePairConfirmB  MsgType = 14
)

type Hello struct {
	ProtoVersion uint16   `cbor:"1,keyasint"`
	Role         string   `cbor:"2,keyasint"`
	AppVersion   string   `cbor:"3,keyasint"`
	Capabilities []string `cbor:"4,keyasint"`
}
type pairValue struct {
	Value [32]byte `cbor:"1,keyasint"`
}
type PairShareA pairValue
type PairShareB pairValue
type PairConfirmA pairValue
type PairConfirmB pairValue
type PairToken struct {
	Token [16]byte `cbor:"1,keyasint"`
}
type PairResult struct {
	Accepted bool   `cbor:"1,keyasint"`
	Reason   string `cbor:"2,keyasint"`
}
type VehicleState struct {
	NIC         string `cbor:"1,keyasint"`
	NICAddr     string `cbor:"2,keyasint"`
	GatewayAddr string `cbor:"3,keyasint"`
	VIN         string `cbor:"4,keyasint"`
	MAC         string `cbor:"5,keyasint"`
	DiagAddr    byte   `cbor:"6,keyasint"`
	IdentRaw    []byte `cbor:"7,keyasint"`
	Seq         uint64 `cbor:"8,keyasint"`
}
type Ping struct {
	Seq    uint64 `cbor:"1,keyasint"`
	SentAt int64  `cbor:"2,keyasint"`
}
type Pong struct {
	Seq    uint64 `cbor:"1,keyasint"`
	SentAt int64  `cbor:"2,keyasint"`
}
type ExposeReverse struct {
	Port   uint16 `cbor:"1,keyasint"`
	Enable bool   `cbor:"2,keyasint"`
}
type ExposeResult struct {
	Port   uint16 `cbor:"1,keyasint"`
	OK     bool   `cbor:"2,keyasint"`
	Reason string `cbor:"3,keyasint"`
}
type Log struct {
	Level string            `cbor:"1,keyasint"`
	Msg   string            `cbor:"2,keyasint"`
	Attrs map[string]string `cbor:"3,keyasint"`
	At    int64             `cbor:"4,keyasint"`
}
type OpenTunnel struct {
	Kind   string `cbor:"1,keyasint"`
	Target string `cbor:"2,keyasint"`
	Origin string `cbor:"3,keyasint"`
}

// Unknown retains the body of an extension message without interpreting it.
type Unknown struct {
	Type MsgType
	Body cbor.RawMessage
}
type Envelope struct {
	Type MsgType         `cbor:"1,keyasint"`
	Body cbor.RawMessage `cbor:"2,keyasint"`
}

var enc, _ = cbor.CanonicalEncOptions().EncMode()
var dec, _ = (cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, MaxNestedLevels: 16, MaxArrayElements: 65536, MaxMapPairs: 65536}).DecMode()

// Write accepts a message value or non-nil pointer; callers serialize writes.
func Write(w io.Writer, msg any) error {
	v := reflect.ValueOf(msg)
	if v.Kind() == reflect.Pointer && !v.IsNil() {
		msg = v.Elem().Interface()
	}
	var typ MsgType
	switch msg.(type) {
	case Hello:
		typ = TypeHello
	case PairShareA:
		typ = TypePairShareA
	case PairShareB:
		typ = TypePairShareB
	case PairConfirmA:
		typ = TypePairConfirmA
	case PairConfirmB:
		typ = TypePairConfirmB
	case PairToken:
		typ = TypePairToken
	case PairResult:
		typ = TypePairResult
	case VehicleState:
		typ = TypeVehicleState
	case Ping:
		typ = TypePing
	case Pong:
		typ = TypePong
	case ExposeReverse:
		typ = TypeExposeReverse
	case ExposeResult:
		typ = TypeExposeResult
	case Log:
		typ = TypeLog
	case OpenTunnel:
		typ = TypeOpenTunnel
	default:
		return fmt.Errorf("proto: unsupported message %T", msg)
	}
	body, err := enc.Marshal(msg)
	if err != nil {
		return fmt.Errorf("proto: encode body: %w", err)
	}
	return writeEnvelope(w, Envelope{Type: typ, Body: body})
}
func writeEnvelope(w io.Writer, e Envelope) error {
	raw, err := enc.Marshal(e)
	if err != nil {
		return fmt.Errorf("proto: encode envelope: %w", err)
	}
	if len(raw) > MaxFrameLen {
		return fmt.Errorf("proto: frame too large: %d", len(raw))
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(raw)))
	for _, b := range [][]byte{prefix[:], raw} {
		for len(b) > 0 {
			n, err := w.Write(b)
			if err != nil {
				return fmt.Errorf("proto: write frame: %w", err)
			}
			if n == 0 {
				return io.ErrShortWrite
			}
			b = b[n:]
		}
	}
	return nil
}

// Read bounds allocation before decoding and consumes exactly one frame.
func Read(r io.Reader, maxLen int) (any, error) {
	return read(r, maxLen, false)
}

// ReadPair bounds allocation and accepts only digital frames or a PairResult.
func ReadPair(r io.Reader) (any, error) { return read(r, 256, true) }

func read(r io.Reader, maxLen int, pairOnly bool) (any, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return nil, fmt.Errorf("proto: read length: %w", err)
	}
	n := binary.BigEndian.Uint32(prefix[:])
	if maxLen <= 0 || n == 0 || uint64(n) > uint64(maxLen) {
		return nil, fmt.Errorf("proto: invalid frame length %d (limit %d)", n, maxLen)
	}
	raw := make([]byte, int(n))
	if _, err := io.ReadFull(r, raw); err != nil {
		return nil, fmt.Errorf("proto: read body: %w", err)
	}
	var e Envelope
	if err := dec.Unmarshal(raw, &e); err != nil {
		return nil, fmt.Errorf("proto: decode envelope: %w", err)
	}
	if e.Type == 0 || len(e.Body) == 0 {
		return nil, fmt.Errorf("proto: missing envelope type or body")
	}
	digital := e.Type >= TypePairShareA && e.Type <= TypePairConfirmB
	if pairOnly && !digital && e.Type != TypePairResult {
		return nil, fmt.Errorf("proto: unexpected pairing type %d", e.Type)
	}
	var msg any
	switch e.Type {
	case TypeHello:
		msg = new(Hello)
	case TypePairShareA:
		msg = new(PairShareA)
	case TypePairShareB:
		msg = new(PairShareB)
	case TypePairConfirmA:
		msg = new(PairConfirmA)
	case TypePairConfirmB:
		msg = new(PairConfirmB)
	case TypePairToken:
		msg = new(PairToken)
	case TypePairResult:
		msg = new(PairResult)
	case TypeVehicleState:
		msg = new(VehicleState)
	case TypePing:
		msg = new(Ping)
	case TypePong:
		msg = new(Pong)
	case TypeExposeReverse:
		msg = new(ExposeReverse)
	case TypeExposeResult:
		msg = new(ExposeResult)
	case TypeLog:
		msg = new(Log)
	case TypeOpenTunnel:
		msg = new(OpenTunnel)
	default:
		return Unknown{Type: e.Type, Body: e.Body}, nil
	}
	if err := dec.Unmarshal(e.Body, msg); err != nil {
		return nil, fmt.Errorf("proto: decode type %d: %w", e.Type, err)
	}
	if digital {
		var canonical bytes.Buffer
		if err := Write(&canonical, msg); err != nil {
			return nil, err
		}
		if !bytes.Equal(raw, canonical.Bytes()[4:]) {
			return nil, fmt.Errorf("proto: noncanonical pairing frame")
		}
	}
	return reflect.ValueOf(msg).Elem().Interface(), nil
}
