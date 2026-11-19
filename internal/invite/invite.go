// Package invite encodes renet1 invitations. The unpadded base32 body is a
// big-endian binary record: version:u8, peer:32 bytes, token:16 bytes,
// expiry:Unix seconds:i64, endpoint count:u16, then each endpoint as
// kind length:u16 + UTF-8 bytes, address length:u16 + UTF-8 bytes, hub:32 bytes.
// Version 1 has no extension or trailing bytes; records are limited to 64 KiB.
package invite

import (
	"bytes"
	"encoding/base32"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"remote-enet/internal/transport"
)

const maxSize = 64 * 1024

var encoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// Invite carries a one-time token; callers must protect the whole code as a secret.
type Invite struct {
	Version   uint8
	Peer      transport.PeerInfo
	Token     [16]byte
	ExpiresAt time.Time
}

// Encode preserves second precision for expiry and never changes Version.
func (i Invite) Encode() (string, error) {
	if i.Version != 1 {
		return "", fmt.Errorf("invite: unsupported version %d", i.Version)
	}
	if len(i.Peer.Endpoints) > 65535 {
		return "", fmt.Errorf("invite: too many endpoints")
	}
	var b bytes.Buffer
	b.WriteByte(i.Version)
	b.Write(i.Peer.ID[:])
	b.Write(i.Token[:])
	binary.Write(&b, binary.BigEndian, i.ExpiresAt.Unix())
	binary.Write(&b, binary.BigEndian, uint16(len(i.Peer.Endpoints)))
	for _, e := range i.Peer.Endpoints {
		for _, s := range []string{e.Kind, e.Addr} {
			if len(s) > 65535 || !utf8.ValidString(s) {
				return "", fmt.Errorf("invite: invalid endpoint string")
			}
			binary.Write(&b, binary.BigEndian, uint16(len(s)))
			b.WriteString(s)
		}
		b.Write(e.Hub[:])
		if b.Len() > maxSize {
			return "", fmt.Errorf("invite: record too large")
		}
	}
	return "renet1:" + encoding.EncodeToString(b.Bytes()), nil
}

// Decode validates expiry against the current clock.
func Decode(s string) (Invite, error) { return DecodeAt(s, time.Now()) }

// DecodeAt treats expiry equal to now as expired. Prefix and payload ignore case.
func DecodeAt(s string, now time.Time) (Invite, error) {
	var i Invite
	if len(s) < 7 || !strings.EqualFold(s[:7], "renet1:") {
		return i, fmt.Errorf("invite: invalid prefix")
	}
	if len(s)-7 > encoding.EncodedLen(maxSize) {
		return i, fmt.Errorf("invite: record too large")
	}
	raw, err := encoding.DecodeString(strings.ToUpper(s[7:]))
	if err != nil {
		return i, fmt.Errorf("invite: base32: %w", err)
	}
	if encoding.EncodeToString(raw) != strings.ToUpper(s[7:]) {
		return i, fmt.Errorf("invite: noncanonical base32")
	}
	r := bytes.NewReader(raw)
	version, err := r.ReadByte()
	if err != nil {
		return i, fmt.Errorf("invite: version: %w", err)
	}
	i.Version = version
	if version != 1 {
		return Invite{}, fmt.Errorf("invite: unsupported version %d", version)
	}
	var expiry int64
	var count uint16
	for _, dst := range []any{&i.Peer.ID, &i.Token, &expiry, &count} {
		if err := binary.Read(r, binary.BigEndian, dst); err != nil {
			return Invite{}, fmt.Errorf("invite: header: %w", err)
		}
	}
	i.ExpiresAt = time.Unix(expiry, 0).UTC()
	if !now.Before(i.ExpiresAt) {
		return Invite{}, ErrInviteExpired
	}
	for range int(count) {
		var e transport.Endpoint
		for _, dst := range []*string{&e.Kind, &e.Addr} {
			var n uint16
			if err := binary.Read(r, binary.BigEndian, &n); err != nil {
				return Invite{}, fmt.Errorf("invite: string length: %w", err)
			}
			if int(n) > r.Len() {
				return Invite{}, fmt.Errorf("invite: truncated string")
			}
			b := make([]byte, int(n))
			if _, err := io.ReadFull(r, b); err != nil {
				return Invite{}, fmt.Errorf("invite: string: %w", err)
			}
			if !utf8.Valid(b) {
				return Invite{}, fmt.Errorf("invite: invalid UTF-8")
			}
			*dst = string(b)
		}
		if _, err := io.ReadFull(r, e.Hub[:]); err != nil {
			return Invite{}, fmt.Errorf("invite: hub: %w", err)
		}
		i.Peer.Endpoints = append(i.Peer.Endpoints, e)
	}
	if r.Len() != 0 {
		return Invite{}, fmt.Errorf("invite: trailing bytes")
	}
	return i, nil
}
