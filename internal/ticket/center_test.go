package ticket

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/identity"
)

type centerVector[I, E any] struct {
	Name   string
	Input  I
	Expect E
}

func readCenterVectors[I, E any](t *testing.T, file string) []centerVector[I, E] {
	t.Helper()
	var doc struct{ Cases []centerVector[I, E] }
	b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "conformance", file))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Cases) == 0 {
		t.Fatal("empty vector file")
	}
	return doc.Cases
}

type issuanceInput struct {
	PublicKey    string `json:"public_key"`
	SigningKey   string `json:"signing_key"`
	TicketMaxTTL int64  `json:"ticket_max_ttl"`
	Now          int64
	Claims       struct {
		V, Role                                      uint8
		Center, Acct, Dev, Peer, Sid, Node, Hub, JTI string
		Iat, Nbf, Exp                                int64
		Gen                                          uint32
	}
}

func (v issuanceInput) claims() CenterClaims {
	c := v.Claims
	return CenterClaims{
		Center:     c.Center,
		Account:    [16]byte(must(hex.DecodeString(c.Acct))),
		Device:     identity.Fingerprint(must(hex.DecodeString(c.Dev))),
		Role:       c.Role,
		Peer:       identity.Fingerprint(must(hex.DecodeString(c.Peer))),
		Session:    [16]byte(must(hex.DecodeString(c.Sid))),
		Node:       [16]byte(must(hex.DecodeString(c.Node))),
		Hub:        identity.Fingerprint(must(hex.DecodeString(c.Hub))),
		IssuedAt:   time.Unix(c.Iat, 0).UTC(),
		NotBefore:  time.Unix(c.Nbf, 0).UTC(),
		Expires:    time.Unix(c.Exp, 0).UTC(),
		JTI:        [16]byte(must(hex.DecodeString(c.JTI))),
		Generation: c.Gen,
	}
}

type issuanceOutput struct {
	Result, Kid, Protected, Payload, Signature, Token string
	SigStructure                                      string `json:"sig_structure"`
	Length                                            int
}

func TestCenterIssuanceVectors(t *testing.T) {
	for _, v := range readCenterVectors[issuanceInput, issuanceOutput](t, "v04-ticket.json") {
		t.Run(v.Name, func(t *testing.T) {
			seed := sha256.Sum256([]byte("renet-conformance-v1/" + v.Input.SigningKey))
			priv := ed25519.NewKeyFromSeed(seed[:])
			pub := priv.Public().(ed25519.PublicKey)
			if hex.EncodeToString(pub) != v.Input.PublicKey {
				t.Fatal("signing key does not match vector public key")
			}
			kid := KidOf(pub)
			lookup := func(k Kid) (ed25519.PublicKey, bool) { return pub, k == kid }
			c := v.Input.claims()
			raw, err := SignCenter(priv, c, v.Input.TicketMaxTTL)
			if err != nil {
				t.Fatal(err)
			}
			parts := partsOf(t, raw)
			protected, payload := parts[0].([]byte), parts[2].([]byte)
			output := issuanceOutput{
				Result:       "accept",
				Kid:          hex.EncodeToString(kid[:]),
				Protected:    hex.EncodeToString(protected),
				Payload:      hex.EncodeToString(payload),
				Signature:    hex.EncodeToString(parts[3].([]byte)),
				Token:        hex.EncodeToString(raw),
				SigStructure: hex.EncodeToString(signatureInput(protected, payload)),
				Length:       len(raw),
			}
			if output != v.Expect || len(raw) > 512 || v.Input.Claims.V != 1 {
				t.Fatal("center issuance vector mismatch")
			}
			expect := CenterExpect{
				Center:       c.Center,
				Node:         c.Node,
				Hub:          c.Hub,
				Generation:   c.Generation,
				TLSDevice:    c.Device,
				Role:         c.Role,
				Target:       c.Peer,
				ClockSkew:    30,
				TicketMaxTTL: v.Input.TicketMaxTTL,
			}
			if _, err := VerifyCenter(raw, nil, expect, time.Unix(v.Input.Now, 0)); err == nil || !strings.HasPrefix(err.Error(), "8.4/3:") {
				t.Fatalf("missing trust roots: %v", err)
			}
			got, err := VerifyCenter(raw, lookup, expect, time.Unix(v.Input.Now, 0))
			if err != nil || got != c {
				t.Fatalf("verified claims %+v: %v", got, err)
			}
			if _, _, err := Verify(raw, lookup); err == nil || !strings.HasPrefix(err.Error(), "A5:") {
				t.Fatalf("self-hosted verifier: %v", err)
			}
		})
	}
}

type admissionInput struct {
	Token  string
	Config struct {
		Center       string `json:"center_id"`
		Node         string `json:"node_id"`
		Hub          string
		Gen          uint32
		ClockSkew    int64 `json:"clock_skew"`
		TicketMaxTTL int64 `json:"ticket_max_ttl"`
		Keys         []struct {
			Kid       string
			PublicKey string `json:"public_key"`
		}
	}
	Route struct {
		Role   uint8
		Target string
	}
	TLSDevice string `json:"tls_device"`
	Now       int64
}

func TestCenterAdmissionVectors(t *testing.T) {
	type outcome struct{ Result, Reason string }
	for _, v := range readCenterVectors[admissionInput, outcome](t, "v08-ticket-reject.json") {
		t.Run(v.Name, func(t *testing.T) {
			input := v.Input
			cfg := input.Config
			keys := map[Kid]ed25519.PublicKey{}
			for _, key := range cfg.Keys {
				kid := Kid(must(hex.DecodeString(key.Kid)))
				pub := ed25519.PublicKey(must(hex.DecodeString(key.PublicKey)))
				if KidOf(pub) != kid {
					t.Fatal("inconsistent fixture key")
				}
				keys[kid] = pub
			}
			lookup := func(k Kid) (ed25519.PublicKey, bool) {
				p, ok := keys[k]
				return p, ok
			}
			expect := CenterExpect{
				Center:       cfg.Center,
				Node:         [16]byte(must(hex.DecodeString(cfg.Node))),
				Hub:          identity.Fingerprint(must(hex.DecodeString(cfg.Hub))),
				Generation:   cfg.Gen,
				TLSDevice:    identity.Fingerprint(must(hex.DecodeString(input.TLSDevice))),
				Role:         input.Route.Role,
				Target:       identity.Fingerprint(must(hex.DecodeString(input.Route.Target))),
				ClockSkew:    cfg.ClockSkew,
				TicketMaxTTL: cfg.TicketMaxTTL,
			}
			raw := must(hex.DecodeString(input.Token))
			_, err := VerifyCenter(raw, lookup, expect, time.Unix(input.Now, 0))
			got := outcome{Result: "accept"}
			if err != nil {
				reason, _, _ := strings.Cut(err.Error(), ":")
				got = outcome{Result: "reject",
					Reason: reason}
			}
			if got != v.Expect {
				t.Fatalf("want %+v, got %+v (%v)", v.Expect, got, err)
			}
			if v.Name == "v10-selfhost-v2" {
				if _, _, err := Verify(raw, lookup); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestSignCenterBounds(t *testing.T) {
	v := readCenterVectors[issuanceInput, issuanceOutput](t, "v04-ticket.json")[0]
	for _, modify := range []func(*CenterClaims){
		func(c *CenterClaims) { c.Center = "" },
		func(c *CenterClaims) { c.Center = strings.Repeat("a", 33) },
		func(c *CenterClaims) { c.Role = 0 },
		func(c *CenterClaims) { c.Peer = identity.Fingerprint{} },
		func(c *CenterClaims) { c.NotBefore = c.IssuedAt.Add(time.Second) },
		func(c *CenterClaims) { c.Expires = c.IssuedAt },
		func(c *CenterClaims) { c.Expires = c.IssuedAt.Add(time.Duration(v.Input.TicketMaxTTL+1) * time.Second) },
		func(c *CenterClaims) { c.Expires = time.Unix(1<<32, 0) },
	} {
		c := v.Input.claims()
		modify(&c)
		if _, err := SignCenter(private(), c, v.Input.TicketMaxTTL); err == nil {
			t.Errorf("signed invalid claims %+v", c)
		}
	}
}
