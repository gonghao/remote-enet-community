package cpace

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"remote-enet/internal/proto"
)

type conformanceCase struct {
	Name          string
	Input, Expect vector
}

func conformanceCases(t *testing.T, file string, count int) []conformanceCase {
	t.Helper()
	data, err := os.ReadFile("../../../testdata/conformance/" + file + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct{ Cases []conformanceCase }
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) != count {
		t.Fatalf("got %d cases, want %d", len(doc.Cases), count)
	}
	return doc.Cases
}

func (v vector) text(t *testing.T, key string) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(v[key], &s); err != nil {
		t.Fatal(key, err)
	}
	return s
}

func TestConformanceCI(t *testing.T) {
	for _, tc := range conformanceCases(t, "v06-pake-ci", 3) {
		t.Run(tc.Name, func(t *testing.T) {
			v, w := tc.Input, tc.Expect
			b := Binding{
				Capability:        v.text(t, "capability"),
				ClientFP:          [32]byte(v.bytes(t, "fp_client")),
				OperatorFP:        [32]byte(v.bytes(t, "fp_operator")),
				HelloClientHash:   sha256.Sum256(v.bytes(t, "hc")),
				HelloOperatorHash: sha256.Sum256(v.bytes(t, "ho")),
			}
			w.check(t, "hc_sha256", b.HelloClientHash[:])
			w.check(t, "ho_sha256", b.HelloOperatorHash[:])
			if b.Capability != proto.PairDirectCapability {
				b.HubFP = [32]byte(v.bytes(t, "hub_fp"))
			}
			if b.Capability == "pair-center/1" {
				w.check(t, "ci", lvCat(
					[]byte("renet-pake-v1"), []byte(suiteID), []byte(b.Capability), []byte("renet/1"),
					b.ClientFP[:], b.OperatorFP[:], b.HelloClientHash[:], b.HelloOperatorHash[:],
					b.HubFP[:], []byte(v.text(t, "center_id")), v.bytes(t, "session_id"),
				))
				return
			}
			ci, err := b.ci()
			if err != nil {
				t.Fatal(err)
			}
			w.check(t, "ci", ci)
		})
	}
}

func TestConformancePairingMessages(t *testing.T) {
	for _, tc := range conformanceCases(t, "v07-pairing-messages", 2) {
		t.Run(tc.Name, func(t *testing.T) {
			v, w := tc.Input, tc.Expect
			sid := v.bytes(t, "sid")
			g, err := generator(v.bytes(t, "prs"), v.bytes(t, "ci"), sid)
			if err != nil {
				t.Fatal(err)
			}
			ya := v.scalar(t, "ya")
			a, b := multiply(ya, g), multiply(v.scalar(t, "yb"), g)
			k, err := sharedPoint(ya, b[:])
			if err != nil {
				t.Fatal(err)
			}
			key := isk(sid, k[:], a[:], nil, b[:], nil)
			digest := commitment(a)
			tags := confirmationTags(Binding{Exporter: [32]byte(sid)}, k, a, b)
			w.check(t, "commit", digest[:])
			w.check(t, "isk", key[:])
			w.check(t, "ta", tags.client[:])
			w.check(t, "tb", tags.operator[:])
			if tags.sas != w.text(t, "sas") {
				t.Fatalf("unexpected SAS %q", tags.sas)
			}
			msgs := []any{
				proto.PakeCommit{Value: digest},
				proto.PakeShareB{Value: b},
				proto.PakeShareA{Value: a},
				proto.PakeConfirmB{Value: tags.operator},
				proto.PakeConfirmA{Value: tags.client},
				proto.PairDecision{Accepted: true, Reason: "accepted"},
			}
			for i, msg := range msgs {
				var frame bytes.Buffer
				if err := proto.Write(&frame, msg); err != nil {
					t.Fatal(err)
				}
				w.check(t, fmt.Sprintf("cbor%d", 11+i), frame.Bytes()[4:])
			}
		})
	}
}
