package center

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"reflect"
	"testing"
)

type vectorRequest struct {
	Key            string
	Kind           string
	Principal      string
	Method         string
	Target         string
	Nonce          string
	Body           string
	CenterID       string `json:"center_id"`
	Client         string `json:"renet_client"`
	IdempotencyKey string `json:"idempotency_key"`
	RawURL         string `json:"raw_url"`
	Timestamp      int64
	Headers        http.Header
}

func TestRequestSignatureVectors(t *testing.T) {
	data, err := os.ReadFile("../../testdata/conformance/v03-request-signature.json")
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Format int
		Vector string
		Cases  []struct {
			Name   string
			Input  vectorRequest
			Expect struct {
				Result, Signature, Reason string
				CanonicalTarget           string `json:"canonical_target"`
				SigningInput              string `json:"signing_input"`
				Headers                   http.Header
			}
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != 1 || doc.Vector != "V3" || len(doc.Cases) == 0 {
		t.Fatal("invalid V3 document")
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			in, want := c.Input, c.Expect
			if in.RawURL != "" {
				got, err := CanonicalTarget(in.RawURL)
				if want.Result == "reject" {
					if err == nil || want.Reason != "bad_request" {
						t.Fatalf("target %q, error %v, want bad_request", got, err)
					}
					return
				}
				if err != nil || got != want.CanonicalTarget || want.Result != "accept" {
					t.Fatalf("target %q, error %v", got, err)
				}
				return
			}
			decode := func(s string) []byte {
				b, err := hex.DecodeString(s)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			r := Request{in.CenterID, in.Method, in.Target, decode(in.Body)}
			a := RequestAuth{in.Timestamp, in.Client, decode(in.Nonce), in.IdempotencyKey}
			principal := decode(in.Principal)
			seed := sha256.Sum256([]byte("renet-conformance-v1/" + in.Key))
			key := ed25519.NewKeyFromSeed(seed[:])
			headers := in.Headers
			if want.Result == "accept" {
				b := SigningInput(r, a, in.Kind, principal)
				if hex.EncodeToString(b) != want.SigningInput {
					t.Fatalf("signing input %x", b)
				}
				var err error
				switch in.Kind {
				case "device":
					headers, err = SignDevice(r, a, key)
				case "node":
					headers, err = SignNode(r, a, key, [16]byte(principal))
				default:
					t.Fatalf("unknown kind %q", in.Kind)
				}
				if err != nil {
					t.Fatal(err)
				}
				sig, err := base64.RawURLEncoding.DecodeString(headers.Get("Renet-Signature"))
				if err != nil || hex.EncodeToString(sig) != want.Signature {
					t.Fatalf("signature %x, error %v", sig, err)
				}
				if !reflect.DeepEqual(headers, want.Headers) {
					t.Fatalf("headers %v, want %v", headers, want.Headers)
				}
			}
			target, err := CanonicalTarget(r.Target)
			if err != nil || target != r.Target {
				if _, err := SignDevice(r, a, key); !errors.Is(err, ErrBadRequest) {
					t.Fatalf("signed noncanonical target: %v", err)
				}
			}
			switch in.Kind {
			case "device":
				err = VerifyDevice(r, headers)
			case "node":
				err = VerifyNode(r, headers, [16]byte(principal), key.Public().(ed25519.PublicKey))
			default:
				t.Fatalf("unknown kind %q", in.Kind)
			}
			switch want.Result {
			case "accept":
				if err != nil {
					t.Fatal(err)
				}
			case "reject":
				reason := map[string]error{"unauthenticated": ErrUnauthenticated, "bad_request": ErrBadRequest}[want.Reason]
				if reason == nil || !errors.Is(err, reason) {
					t.Fatalf("error %v, want %s", err, want.Reason)
				}
			default:
				t.Fatalf("unknown result %q", want.Result)
			}
		})
	}
}
