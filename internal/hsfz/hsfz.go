// Package hsfz encodes the six-byte HSFZ envelope without interpreting UDS.
package hsfz

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
)

type Control uint16

const (
	ControlDiag                      Control = 0x0001
	ControlAck                       Control = 0x0002
	ControlTerminal15                Control = 0x0010
	ControlVehicleIdent              Control = 0x0011
	ControlAliveCheck                Control = 0x0012
	ControlStatusInquiry             Control = 0x0013
	ControlErrIncorrectTesterAddress Control = 0x0040
	ControlErrIncorrectControlWord   Control = 0x0041
	ControlErrIncorrectFormat        Control = 0x0042
	ControlErrIncorrectDestAddress   Control = 0x0043
	ControlErrMessageTooLarge        Control = 0x0044
	ControlErrAppNotReady            Control = 0x0045
	ControlErrOutOfMemory            Control = 0x00FF
)

// Frame owns only the envelope; empty bodies and unknown controls are valid.
type Frame struct {
	Control Control
	Body    []byte
}

func (f Frame) MarshalBinary() ([]byte, error) {
	if uint64(len(f.Body)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("HSFZ body too large: %d", len(f.Body))
	}
	b := make([]byte, 6+len(f.Body))
	binary.BigEndian.PutUint32(b, uint32(len(f.Body)))
	binary.BigEndian.PutUint16(b[4:], uint16(f.Control))
	copy(b[6:], f.Body)
	return b, nil
}

// ReadFrame rejects oversized bodies before allocating, including for unknown controls.
func ReadFrame(r io.Reader, maxBody int) (Frame, error) {
	var h [6]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return Frame{}, fmt.Errorf("read HSFZ header: %w", err)
	}
	n := binary.BigEndian.Uint32(h[:])
	if maxBody < 0 || uint64(n) > uint64(maxBody) {
		return Frame{}, fmt.Errorf("HSFZ body length %d exceeds limit %d", n, maxBody)
	}
	f := Frame{Control: Control(binary.BigEndian.Uint16(h[4:])), Body: make([]byte, int(n))}
	if _, err := io.ReadFull(r, f.Body); err != nil {
		return Frame{}, fmt.Errorf("read HSFZ body: %w", err)
	}
	return f, nil
}
func WriteFrame(w io.Writer, f Frame) error {
	b, err := f.MarshalBinary()
	if err != nil {
		return err
	}
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return fmt.Errorf("write HSFZ frame: %w", err)
		}
		if n == 0 {
			return fmt.Errorf("write HSFZ frame: %w", io.ErrShortWrite)
		}
		b = b[n:]
	}
	return nil
}

// DiagBody applies only to diagnostic and ACK frames.
type DiagBody struct {
	Src, Dst byte
	Payload  []byte
}

func ParseDiagBody(body []byte) (DiagBody, error) {
	if len(body) < 2 {
		return DiagBody{}, fmt.Errorf("HSFZ diagnostic body needs two addresses: got %d bytes", len(body))
	}
	return DiagBody{body[0], body[1], bytes.Clone(body[2:])}, nil
}
func (d DiagBody) Marshal() []byte { return append([]byte{d.Src, d.Dst}, d.Payload...) }

var IdentRequest = []byte{0, 0, 0, 0, 0, 0x11}

// Raw preserves the received body, including unknown extension fields.
type Identification struct {
	DiagAddr byte
	MAC      [6]byte
	VIN      string
	Raw      []byte
}

func ParseIdentification(body []byte) (Identification, error) {
	id := Identification{Raw: bytes.Clone(body)}
	field := func(tag string, n int) ([]byte, error) {
		i := bytes.Index(body, []byte(tag))
		if i < 0 || len(body)-i-len(tag) < n {
			return nil, fmt.Errorf("missing or truncated identification field %s", tag)
		}
		return body[i+len(tag) : i+len(tag)+n], nil
	}
	a, err := field("DIAGADR", 2)
	if err != nil {
		return Identification{}, err
	}
	addr, err := hex.DecodeString(string(a))
	if err != nil {
		return Identification{}, fmt.Errorf("decode DIAGADR: %w", err)
	}
	id.DiagAddr = addr[0]
	m, err := field("BMWMAC", 12)
	if err != nil {
		return Identification{}, err
	}
	mac, err := hex.DecodeString(string(m))
	if err != nil {
		return Identification{}, fmt.Errorf("decode BMWMAC: %w", err)
	}
	copy(id.MAC[:], mac)
	v, err := field("BMWVIN", 17)
	if err != nil {
		return Identification{}, err
	}
	for _, c := range v {
		if c < 0x20 || c > 0x7e {
			return Identification{}, fmt.Errorf("BMWVIN contains non-printable ASCII")
		}
	}
	id.VIN = string(v)
	return id, nil
}

// MarshalBody updates known fields in Raw when present, preserving extension bytes.
// Callers must supply a 17-byte printable ASCII VIN.
func (id Identification) MarshalBody() []byte {
	fields := []struct{ tag, value string }{{"DIAGADR", fmt.Sprintf("%02X", id.DiagAddr)}, {"BMWMAC", strings.ToUpper(hex.EncodeToString(id.MAC[:]))}, {"BMWVIN", id.VIN}}
	b := bytes.Clone(id.Raw)
	if _, err := ParseIdentification(b); err != nil {
		b = nil
		for _, f := range fields {
			b = append(b, f.tag+f.value...)
		}
		return b
	}
	for _, f := range fields {
		i := bytes.Index(b, []byte(f.tag)) + len(f.tag)
		n := len(f.value)
		if f.tag == "BMWVIN" {
			n = 17
		}
		b = append(append(bytes.Clone(b[:i]), []byte(f.value)...), b[i+n:]...)
	}
	return b
}
func AliveResponse(tester byte) Frame { return Frame{ControlStatusInquiry, []byte{0, tester}} }

// AckFor retains the request direction and at most five UDS bytes.
func AckFor(req DiagBody) Frame {
	req.Payload = req.Payload[:min(len(req.Payload), 5)]
	return Frame{ControlAck, req.Marshal()}
}
