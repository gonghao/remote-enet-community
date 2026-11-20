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

// PairHubCapability binds numeric pairing to the observed Hub fingerprint.
const PairHubCapability = "pair-hub/1"

// PairCenterCapability binds numeric pairing to a Hub, center and session.
const PairCenterCapability = "pair-center/1"

// HubTicketCapability negotiates device ticket delivery after pairing.
const HubTicketCapability = "hub-ticket/1"

const maxHubTicketFrame = 1100

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
	TypePakeCommit    MsgType = 11
	TypePakeShareB    MsgType = 12
	TypePakeShareA    MsgType = 13
	TypePakeConfirmB  MsgType = 14
	TypePakeConfirmA  MsgType = 15
	TypePairDecision  MsgType = 16
	TypeHubTicket     MsgType = 17
)

type Hello struct {
	ProtoVersion uint16   `cbor:"1,keyasint"`
	Role         string   `cbor:"2,keyasint"`
	AppVersion   string   `cbor:"3,keyasint"`
	Capabilities []string `cbor:"4,keyasint"`
}

// HubTicket carries the opaque device ticket in an inner control frame.
type HubTicket struct {
	Ticket []byte `cbor:"1,keyasint"`
}

type pairValue struct {
	Value [32]byte `cbor:"1,keyasint"`
}
type PakeShareA pairValue
type PakeShareB pairValue
type PakeCommit struct {
	Value [64]byte `cbor:"1,keyasint"`
}
type PairDecision PairResult
type PakeConfirmA PakeCommit
type PakeConfirmB PakeCommit
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
	switch m := msg.(type) {
	case HubTicket:
		if len(m.Ticket) < 1 || len(m.Ticket) > 1024 {
			return fmt.Errorf("proto: invalid Hub ticket length %d", len(m.Ticket))
		}
		typ = TypeHubTicket
	case Hello:
		typ = TypeHello
	case PakeCommit:
		typ = TypePakeCommit
	case PairDecision:
		typ = TypePairDecision
	case PakeShareA:
		typ = TypePakeShareA
	case PakeShareB:
		typ = TypePakeShareB
	case PakeConfirmA:
		typ = TypePakeConfirmA
	case PakeConfirmB:
		typ = TypePakeConfirmB
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

// ReadPair bounds allocation and accepts only messages 11 through 16.
func ReadPair(r io.Reader) (any, error) { return read(r, 256, true) }

// ReadHubTicket checks the negotiated ticket frame limit before allocation.
func ReadHubTicket(r io.Reader) (HubTicket, error) {
	msg, err := Read(r, maxHubTicketFrame)
	if err != nil {
		return HubTicket{}, err
	}
	ticket, ok := msg.(HubTicket)
	if !ok {
		return HubTicket{}, fmt.Errorf("proto: expected HubTicket, got %T", msg)
	}
	return ticket, nil
}

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
	digital := e.Type >= TypePakeCommit && e.Type <= TypePairDecision
	if pairOnly && !digital {
		return nil, fmt.Errorf("proto: unexpected pairing type %d", e.Type)
	}
	var msg any
	switch e.Type {
	case TypeHubTicket:
		if len(raw) > maxHubTicketFrame {
			return nil, fmt.Errorf("proto: Hub ticket frame too large: %d", len(raw))
		}
		msg = new(HubTicket)
	case TypeHello:
		msg = new(Hello)
	case TypePakeCommit:
		msg = new(PakeCommit)
	case TypePairDecision:
		msg = new(PairDecision)
	case TypePakeShareA:
		msg = new(PakeShareA)
	case TypePakeShareB:
		msg = new(PakeShareB)
	case TypePakeConfirmA:
		msg = new(PakeConfirmA)
	case TypePakeConfirmB:
		msg = new(PakeConfirmB)
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
	if digital || e.Type == TypeHubTicket {
		var canonical bytes.Buffer
		if err := Write(&canonical, msg); err != nil {
			return nil, err
		}
		if !bytes.Equal(raw, canonical.Bytes()[4:]) {
			return nil, fmt.Errorf("proto: noncanonical control frame")
		}
	}
	return reflect.ValueOf(msg).Elem().Interface(), nil
}
