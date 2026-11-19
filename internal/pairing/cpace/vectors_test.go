package cpace

import (
	"bytes"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/gtank/ristretto255"

	"remote-enet/internal/proto"
)

type vector map[string]json.RawMessage

func readVector(t *testing.T, name string) vector {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (v vector) bytes(t *testing.T, key string) []byte {
	t.Helper()
	var value string
	if err := json.Unmarshal(v[key], &value); err != nil {
		t.Fatal(key, err)
	}
	data, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(key, err)
	}
	return data
}
func (v vector) check(t *testing.T, key string, data []byte) {
	t.Helper()
	if !bytes.Equal(v.bytes(t, key), data) {
		t.Fatalf("vector mismatch: %s", key)
	}
}
func (v vector) scalar(t *testing.T, key string) *ristretto255.Scalar {
	t.Helper()
	s, err := ristretto255.NewScalar().SetCanonicalBytes(v.bytes(t, key))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (v vector) binding(t *testing.T) Binding {
	t.Helper()
	return Binding{
		Exporter:          [32]byte(v.bytes(t, "sid")),
		ClientFP:          [32]byte(v.bytes(t, "fpA")),
		OperatorFP:        [32]byte(v.bytes(t, "fpB")),
		HelloClientHash:   sha256.Sum256(v.bytes(t, "HC")),
		HelloOperatorHash: sha256.Sum256(v.bytes(t, "HO")),
	}
}

func TestGeneratorVector(t *testing.T) {
	v := readVector(t, "generator")
	prs, ci, sid := v.bytes(t, "PRS"), v.bytes(t, "CI"), v.bytes(t, "sid")
	input := generatorString(prs, ci, sid)
	v.check(t, "generator_string(G.DSI,PRS,CI,sid,H.s_in_bytes)", input)
	digest := sha512.Sum512(input)
	v.check(t, "hash result", digest[:])
	g, err := generator(prs, ci, sid)
	if err != nil {
		t.Fatal(err)
	}
	v.check(t, "encoded generator g", g.Bytes())
}

func TestExchangeVectors(t *testing.T) {
	for _, name := range []string{"exchange", "application"} {
		t.Run(name, func(t *testing.T) {
			v := readVector(t, name)
			prs, ci, sid := v.bytes(t, "PRS"), v.bytes(t, "CI"), v.bytes(t, "sid")
			g, err := generator(prs, ci, sid)
			if err != nil {
				t.Fatal(err)
			}
			v.check(t, "g", g.Bytes())
			ya, yb := v.scalar(t, "ya"), v.scalar(t, "yb")
			a, y := multiply(ya, g), multiply(yb, g)
			v.check(t, "Ya", a[:])
			v.check(t, "Yb", y[:])
			k, err := sharedPoint(ya, y[:])
			if err != nil {
				t.Fatal(err)
			}
			v.check(t, "K", k[:])
			kb, err := sharedPoint(yb, a[:])
			if err != nil {
				t.Fatal(err)
			}
			v.check(t, "K", kb[:])
			var adA, adB []byte
			if name == "exchange" {
				adA, adB = v.bytes(t, "ADa"), v.bytes(t, "ADb")
			}
			key := isk(sid, k[:], a[:], adA, y[:], adB)
			v.check(t, "ISK_IR", key[:])
			if name == "application" {
				b := v.binding(t)
				v.check(t, "CI", b.ci())
				digest := commitment(a)
				v.check(t, "commit", digest[:])
				tags := confirmationTags(b, k, a, y)
				v.check(t, "Ta", tags.client[:])
				v.check(t, "Tb", tags.operator[:])
				var sas string
				if err := json.Unmarshal(v["SAS"], &sas); err != nil || sas != tags.sas || sas[0] != '0' {
					t.Fatal("SAS mismatch")
				}
				for j, msg := range []any{proto.PakeCommit{Value: digest}, proto.PakeShareB{Value: y}, proto.PakeShareA{Value: a}, proto.PakeConfirmB{Value: tags.operator}, proto.PakeConfirmA{Value: tags.client}, proto.PairDecision{Accepted: true, Reason: "accepted"}} {
					var frame bytes.Buffer
					if err := proto.Write(&frame, msg); err != nil {
						t.Fatal(err)
					}
					v.check(t, fmt.Sprintf("CBOR%d", 11+j), frame.Bytes()[4:])
				}
			}
		})
	}
}

func TestPointVectors(t *testing.T) {
	v := readVector(t, "points")
	var valid vector
	if err := json.Unmarshal(v["Valid"], &valid); err != nil {
		t.Fatal(err)
	}
	s := valid.scalar(t, "s")
	x := valid.bytes(t, "X")
	k, err := sharedPoint(s, x)
	if err != nil {
		t.Fatal(err)
	}
	valid.check(t, "G.scalar_mult_vfy(s,X)", k[:])
	const decodeError = "decode CPace peer share: invalid encoding"
	for _, tc := range []struct {
		share []byte
		err   string
	}{
		{v.bytes(t, "Invalid Y1"), decodeError},
		{v.bytes(t, "Invalid Y2"), "validate CPace peer share: identity point"},
		{x[:31], decodeError},
		{append(x, 0), decodeError},
		{bytes.Repeat([]byte{255}, 32), decodeError},
	} {
		if _, err := sharedPoint(s, tc.share); err == nil || err.Error() != tc.err {
			t.Fatalf("sharedPoint: got %v, want %q", err, tc.err)
		}
		if len(tc.share) == 32 {
			p := [32]byte(tc.share)
			r, _, err := Respond(exampleCode, testBinding(), commitment(p))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Open(p); err == nil {
				t.Fatal("respond accepted invalid share")
			}
			i, _ := startTest(t, testBinding())
			if _, err := i.Reveal(p); err == nil {
				t.Fatal("confirm accepted invalid share")
			}
			if _, err := i.Reveal([32]byte(x)); err == nil {
				t.Fatal("retried invalid share")
			}
		}
	}
}

func TestScalarSampling(t *testing.T) {
	input := make([]byte, 64)
	input[31] = 0xf0
	input[32] = 1
	s, err := sampleScalar(bytes.NewReader(input))
	if err != nil || !bytes.Equal(s.Bytes(), input[32:]) {
		t.Fatal("zero scalar was not discarded")
	}
	maskInput := [32]byte{31: 0xff}
	masked, err := sampleScalar(bytes.NewReader(maskInput[:]))
	if err != nil || [32]byte(masked.Bytes()) != [32]byte{31: 0x0f} {
		t.Fatal("scalar was not masked to 252 bits")
	}
	b := testBinding()
	_, first := startTest(t, b)
	_, second := startTest(t, b)
	if first == second {
		t.Fatal("Start reused the public share")
	}
}

func TestCenterCIVector(t *testing.T) {
	v := readVector(t, "application")
	var center vector
	if err := json.Unmarshal(v["center"], &center); err != nil {
		t.Fatal(err)
	}
	hc := sha256.Sum256(center.bytes(t, "HC"))
	ho := sha256.Sum256(center.bytes(t, "HO"))
	center.check(t, "CI", lvCat([]byte("renet-pake-v1"), []byte(suiteID), []byte("pair-center/1"), []byte("renet/1"), v.bytes(t, "fpA"), v.bytes(t, "fpB"), hc[:], ho[:], center.bytes(t, "hub_fp"), center.bytes(t, "center_id"), center.bytes(t, "session_id")))
}
