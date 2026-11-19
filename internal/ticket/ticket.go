// Package ticket implements the self-hosted v2 COSE ticket format.
package ticket

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"

	"github.com/fxamacker/cbor/v2"

	"remote-enet/internal/identity"
)

// Kid identifies an issuer within the Hub's configured trust roots.
type Kid [8]byte

// KidOf hashes the raw Ed25519 public key.
func KidOf(pub ed25519.PublicKey) Kid {
	h := sha256.Sum256(pub)
	return Kid(h[:8])
}

// Claims uses zero Device, Peer and PairUntil to omit optional fields on signing.
type Claims struct {
	Device, Peer, Hub                       identity.Fingerprint
	Role                                    uint8
	IssuedAt, NotBefore, Expires, PairUntil time.Time
	JTI                                     [16]byte
	Bearer                                  bool
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

var enc = must(cbor.CoreDetEncOptions().EncMode())
var strict = must(cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, MaxNestedLevels: 16}.DecMode())
var untagged = must(cbor.DecOptions{DupMapKey: cbor.DupMapKeyEnforcedAPF, IndefLength: cbor.IndefLengthForbidden, MaxNestedLevels: 16, TagsMd: cbor.TagsForbidden}.DecMode())

const (
	roleDial = 1
	roleWait = 2
)

// Sign enforces the field profile and issues tickets at Unix-second precision.
func Sign(priv ed25519.PrivateKey, c Claims) ([]byte, error) {
	if len(priv) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid issuer private key length")
	}
	if c.Hub.IsZero() || c.Device.IsZero() != c.Bearer {
		return nil, errors.New("missing hub or inconsistent device binding")
	}
	switch c.Role {
	case roleDial:
		if c.Peer.IsZero() || !c.PairUntil.IsZero() {
			return nil, errors.New("dial requires a peer and forbids a pairing window")
		}
	case roleWait:
		if !c.Peer.IsZero() || c.Bearer {
			return nil, errors.New("wait forbids peer and bearer")
		}
	default:
		return nil, errors.New("invalid ticket role")
	}
	if c.IssuedAt.Unix() != c.NotBefore.Unix() {
		return nil, errors.New("signing requires iat equal to nbf")
	}
	if err := CheckTime(c, c.IssuedAt, 0, math.MaxUint32, math.MaxUint32, math.MaxUint32); err != nil {
		return nil, err
	}
	fields := map[uint64]any{1: 2, 5: c.Role, 9: c.Hub[:], 10: c.IssuedAt.Unix(), 11: c.NotBefore.Unix(), 12: c.Expires.Unix(), 13: c.JTI[:]}
	if c.Bearer {
		fields[16] = true
	} else {
		fields[4] = c.Device[:]
	}
	if !c.Peer.IsZero() {
		fields[6] = c.Peer[:]
	}
	if !c.PairUntil.IsZero() {
		fields[15] = c.PairUntil.Unix()
	}
	kid := KidOf(priv.Public().(ed25519.PublicKey))
	protected := must(enc.Marshal(map[int]any{1: -8, 4: kid[:]}))
	payload := must(enc.Marshal(fields))
	sig := ed25519.Sign(priv, signatureInput(protected, payload))
	return enc.Marshal(cbor.Tag{Number: 18, Content: []any{protected, map[int]int{}, payload, sig}})
}

func signatureInput(protected, payload []byte) []byte {
	return must(enc.Marshal([]any{"Signature1", protected, []byte{}, payload}))
}

// Verify authenticates the original encoded bytes before interpreting the claims.
func Verify(raw []byte, key func(Kid) (ed25519.PublicKey, bool)) (Kid, Claims, error) {
	if key == nil {
		return Kid{}, Claims{}, errors.New("missing issuer lookup")
	}
	return parse(raw, key)
}

// Inspect returns unauthenticated claims suitable only for display and hints.
func Inspect(raw []byte) (Kid, Claims, error) { return parse(raw, nil) }

func parse(raw []byte, key func(Kid) (ed25519.PublicKey, bool)) (Kid, Claims, error) {
	var tag cbor.RawTag
	if len(raw) > 1024 || strict.Unmarshal(raw, &tag) != nil || tag.Number != 18 {
		return Kid{}, Claims{}, errors.New("A2: invalid COSE tag or size")
	}
	var parts []any
	if untagged.Unmarshal(tag.Content, &parts) != nil || len(parts) != 4 {
		return Kid{}, Claims{}, errors.New("A2: invalid COSE array")
	}
	protected, pok := parts[0].([]byte)
	unprotected, uok := parts[1].(map[any]any)
	payload, bok := parts[2].([]byte)
	sig, sok := parts[3].([]byte)
	var headers map[any]any
	if !pok || !uok || len(unprotected) != 0 || !bok || !sok || len(sig) != ed25519.SignatureSize || untagged.Unmarshal(protected, &headers) != nil || len(headers) != 2 || headers[uint64(1)] != int64(-8) {
		return Kid{}, Claims{}, errors.New("A2: invalid COSE headers or signature format")
	}
	id, ok := headers[uint64(4)].([]byte)
	if !ok || len(id) != 8 {
		return Kid{}, Claims{}, errors.New("A2: invalid kid")
	}
	if !bytes.Equal(protected, must(enc.Marshal(headers))) || !bytes.Equal(raw, must(enc.Marshal(cbor.Tag{Number: 18, Content: parts}))) {
		return Kid{}, Claims{}, errors.New("A2: non-deterministic COSE encoding")
	}
	kid := Kid(id)
	if key != nil {
		pub, ok := key(kid)
		if !ok || len(pub) != ed25519.PublicKeySize || KidOf(pub) != kid {
			return Kid{}, Claims{}, errors.New("A3: unknown issuer")
		}
		if !ed25519.Verify(pub, signatureInput(protected, payload), sig) {
			return Kid{}, Claims{}, errors.New("A4: invalid signature")
		}
	}
	c, err := decodeClaims(payload)
	if err != nil {
		return Kid{}, Claims{}, fmt.Errorf("A5: %w", err)
	}
	return kid, c, nil
}

// headSize is called only on a well-formed definite-length CBOR item.
func headSize(item []byte) int {
	ai := item[0] & 31
	if ai < 24 {
		return 1
	}
	return 1 + 1<<(ai-24)
}

// A wider argument is minimal only if it would overflow the preceding width.
func minimalHead(head []byte) bool {
	switch len(head) {
	case 2:
		return head[1] >= 24
	case 3:
		return head[1] != 0
	case 5:
		return head[1] != 0 || head[2] != 0
	case 9:
		return binary.BigEndian.Uint32(head[1:5]) != 0
	default:
		return true
	}
}

// children preserves tags and exact key types that generic Go decoding can discard.
func children(item []byte) ([][]byte, error) {
	body := item[headSize(item):]
	dec := strict.NewDecoder(bytes.NewReader(body))
	var items [][]byte
	for dec.NumBytesRead() < len(body) {
		start := dec.NumBytesRead()
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		items = append(items, body[start:dec.NumBytesRead()])
	}
	return items, nil
}

// Extensions can contain arbitrary CBOR, including maps with compound keys.
// Compare the encoded keys of these deterministic maps without interpreting tags.
func validateInfo(item []byte) error {
	major := item[0] >> 5
	if major == 7 && item[0] >= 0xf9 {
		var f float64
		if err := untagged.Unmarshal(item, &f); err != nil {
			return err
		}
		if !bytes.Equal(item, must(enc.Marshal(f))) {
			return errors.New("non-deterministic information float")
		}
	} else if !minimalHead(item[:headSize(item)]) {
		return errors.New("non-minimal information item")
	}
	switch major {
	case 3:
		if !utf8.Valid(item[headSize(item):]) {
			return errors.New("invalid information text")
		}
	case 4, 5, 6:
		parts, err := children(item)
		if err != nil {
			return err
		}
		for i, v := range parts {
			if major == 5 && i >= 2 && i%2 == 0 && bytes.Compare(parts[i-2], v) >= 0 {
				return errors.New("duplicate or unordered information key")
			}
			if err := validateInfo(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func decodeClaims(payload []byte) (Claims, error) {
	var c Claims
	fields := make(map[uint64]any)
	if len(payload) == 0 || payload[0]>>5 != 5 {
		return c, errors.New("expected claims map")
	}
	if err := strict.Wellformed(payload); err != nil {
		return c, err
	}
	parts, err := children(payload)
	if err != nil {
		return c, err
	}
	for i := 0; i < len(parts); i += 2 {
		var k uint64
		if parts[i][0]>>5 != 0 || untagged.Unmarshal(parts[i], &k) != nil {
			return c, errors.New("expected unsigned field key")
		}
		if fields[k] != nil {
			return c, fmt.Errorf("duplicate field %d", k)
		}
		v := parts[i+1]
		switch k {
		case 1, 5, 10, 11, 12, 15:
			var n uint64
			if v[0]>>5 != 0 || untagged.Unmarshal(v, &n) != nil || n > math.MaxUint32 {
				return c, fmt.Errorf("invalid integer field %d", k)
			}
			fields[k] = n
			switch k {
			case 1:
				if n != 2 {
					return c, errors.New("invalid ticket version")
				}
			case 5:
				if n != roleDial && n != roleWait {
					return c, errors.New("invalid role")
				}
				c.Role = uint8(n)
			case 10:
				c.IssuedAt = time.Unix(int64(n), 0).UTC()
			case 11:
				c.NotBefore = time.Unix(int64(n), 0).UTC()
			case 12:
				c.Expires = time.Unix(int64(n), 0).UTC()
			case 15:
				c.PairUntil = time.Unix(int64(n), 0).UTC()
			}
		case 4, 6, 9, 13:
			dst := map[uint64][]byte{4: c.Device[:], 6: c.Peer[:], 9: c.Hub[:], 13: c.JTI[:]}[k]
			var b []byte
			if untagged.Unmarshal(v, &b) != nil || v[0]>>5 != 2 || len(b) != len(dst) {
				return c, fmt.Errorf("invalid byte string field %d", k)
			}
			fields[k] = b
			copy(dst, b)
		case 16:
			if len(v) != 1 || v[0] != 0xf5 {
				return c, errors.New("invalid bearer flag")
			}
			fields[k] = true
			c.Bearer = true
		default:
			if k < 64 {
				return c, fmt.Errorf("unknown constraint %d", k)
			}
			if err := validateInfo(v); err != nil {
				return c, err
			}
			fields[k] = cbor.RawMessage(v)
		}
	}
	canonical, err := enc.Marshal(fields)
	if err != nil {
		return c, fmt.Errorf("encode canonical claims: %w", err)
	}
	if !bytes.Equal(payload, canonical) {
		return c, errors.New("non-deterministic claims encoding")
	}
	for _, k := range []uint64{1, 5, 9, 10, 11, 12, 13} {
		if fields[k] == nil {
			return c, fmt.Errorf("missing field %d", k)
		}
	}
	if (fields[4] == nil) != c.Bearer || (fields[6] != nil) != (c.Role == roleDial) || (c.Role == roleWait && c.Bearer) || (c.Role == roleDial && fields[15] != nil) {
		return c, errors.New("invalid field combination")
	}
	return c, nil
}

// CheckTime applies A10 using signed limits expressed in seconds.
func CheckTime(c Claims, now time.Time, skew, maxTTL, bootstrapMaxTTL, pairingWindowMax int64) error {
	i, n, e := c.IssuedAt.Unix(), c.NotBefore.Unix(), c.Expires.Unix()
	if skew < 0 || maxTTL < 0 || bootstrapMaxTTL < 0 || pairingWindowMax < 0 || i < 0 || i > n || n >= e || e > math.MaxUint32 {
		return errors.New("A10: invalid time bounds")
	}
	// Comparing now to n-skew also implies i <= now+skew since i <= n.
	s := now.Unix()
	if s < n-skew || (s > e && s-e > skew) || e-i > maxTTL || (c.Bearer && e-i > bootstrapMaxTTL) {
		return errors.New("A10: ticket outside validity window or TTL")
	}
	if !c.PairUntil.IsZero() {
		p := c.PairUntil.Unix()
		if p < n || p > e || p-i > pairingWindowMax {
			return errors.New("A10: invalid pairing window")
		}
	}
	return nil
}
