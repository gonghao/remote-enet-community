package cpace

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/gtank/ristretto255"

	"remote-enet/internal/identity"
)

const (
	suiteID = "cpace21-r255-sha512-ir"
	dsi     = "CPaceRistretto255"
)

// Binding uses the inner TLS exporter, full peer fingerprints and hashes of the exact framed Hellos.
type Binding struct {
	Exporter                           [32]byte
	ClientFP, OperatorFP               identity.Fingerprint
	HelloClientHash, HelloOperatorHash [32]byte
}

func (b Binding) ci() []byte {
	return lengthPrefixed(
		[]byte("renet/1"), []byte("pair-direct/1"), []byte(suiteID),
		[]byte("client"), b.ClientFP[:], []byte("operator"), b.OperatorFP[:],
		b.HelloClientHash[:], b.HelloOperatorHash[:],
	)
}

func lengthPrefixed(parts ...[]byte) []byte {
	var buf []byte
	for _, p := range parts {
		buf = binary.BigEndian.AppendUint32(buf, uint32(len(p)))
		buf = append(buf, p...)
	}
	return buf
}

// The draft uses unsigned LEB128 lengths, including for empty AD.
func lvCat(parts ...[]byte) []byte {
	var buf []byte
	for _, p := range parts {
		buf = binary.AppendUvarint(buf, uint64(len(p)))
		buf = append(buf, p...)
	}
	return buf
}

func generatorString(prs, ci, sid []byte) []byte {
	prefix := lvCat([]byte(dsi), prs)
	pad := make([]byte, max(0, sha512.BlockSize-len(prefix)-1))
	return append(prefix, lvCat(pad, ci, sid)...)
}

func generator(prs, ci, sid []byte) (*ristretto255.Element, error) {
	digest := sha512.Sum512(generatorString(prs, ci, sid))
	g, err := ristretto255.NewIdentityElement().SetUniformBytes(digest[:])
	if err != nil {
		return nil, errors.New("derive CPace generator: uniform mapping failed")
	}
	if g.Equal(ristretto255.NewIdentityElement()) == 1 {
		return nil, errors.New("derive CPace generator: identity point")
	}
	return g, nil
}

func sampleScalar(source io.Reader) (*ristretto255.Scalar, error) {
	for {
		var raw [32]byte
		if _, err := io.ReadFull(source, raw[:]); err != nil {
			return nil, fmt.Errorf("sample CPace scalar: %w", err)
		}
		// Draft section 8.3 samples the low 252 bits in little-endian order.
		raw[31] &= 0x0f
		scalar, err := ristretto255.NewScalar().SetCanonicalBytes(raw[:])
		if err != nil {
			return nil, fmt.Errorf("decode CPace scalar: %w", err)
		}
		if scalar.Equal(ristretto255.NewScalar()) == 0 {
			return scalar, nil
		}
	}
}

func share(code string, b Binding) (*ristretto255.Scalar, [32]byte, error) {
	if len(code) != 8 {
		return nil, [32]byte{}, errCode
	}
	for i := range len(code) {
		if code[i] < '0' || code[i] > '9' {
			return nil, [32]byte{}, errCode
		}
	}
	if b.Exporter == [32]byte{} || b.ClientFP.IsZero() || b.OperatorFP.IsZero() {
		return nil, [32]byte{}, errBinding
	}
	g, err := generator([]byte(code), b.ci(), b.Exporter[:])
	if err != nil {
		return nil, [32]byte{}, err
	}
	s, err := sampleScalar(rand.Reader)
	if err != nil {
		return nil, [32]byte{}, err
	}
	return s, multiply(s, g), nil
}

func multiply(s *ristretto255.Scalar, p *ristretto255.Element) [32]byte {
	return [32]byte(ristretto255.NewIdentityElement().ScalarMult(s, p).Bytes())
}

func sharedPoint(s *ristretto255.Scalar, peer []byte) ([32]byte, error) {
	p, err := ristretto255.NewIdentityElement().SetCanonicalBytes(peer)
	if err != nil {
		return [32]byte{}, errors.New("decode CPace peer share: invalid encoding")
	}
	if p.Equal(ristretto255.NewIdentityElement()) == 1 {
		return [32]byte{}, errors.New("validate CPace peer share: identity point")
	}
	return multiply(s, p), nil
}

func isk(sid, shared, a, adA, b, adB []byte) [64]byte {
	prefix := lvCat([]byte(dsi+"_ISK"), sid, shared)
	return sha512.Sum512(append(prefix, lvCat(a, adA, b, adB)...))
}

func transcript(b Binding, a, y [32]byte) [32]byte {
	return sha256.Sum256(lengthPrefixed([]byte("renet-pair-confirm/1"), b.Exporter[:], b.ci(), a[:], y[:]))
}

type tags struct{ client, operator [32]byte }

func confirmationTags(b Binding, shared, a, y [32]byte) tags {
	key := isk(b.Exporter[:], shared[:], a[:], nil, y[:], nil)
	t := transcript(b, a, y)
	// SHA-256 and a single 32-byte output block satisfy the HKDF limits.
	prk, err := hkdf.Extract(sha256.New, key[:], t[:])
	if err != nil {
		panic(err)
	}
	tag := func(role string) [32]byte {
		info := lengthPrefixed([]byte("confirm"), []byte(role))
		k, err := hkdf.Expand(sha256.New, prk, string(info), sha256.Size)
		if err != nil {
			panic(err)
		}
		mac := hmac.New(sha256.New, k)
		mac.Write(t[:])
		return [32]byte(mac.Sum(nil))
	}
	return tags{tag("client"), tag("operator")}
}
