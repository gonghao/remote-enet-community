package center

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"remote-enet/internal/identity"
)

func TestSessionWireCompatibility(t *testing.T) {
	id := SessionID{1}
	fp := identity.Fingerprint{2}
	node := fmt.Sprintf(`{"node_id":%q,"region":"any","addr":"localhost","port":9000,"hub_fp":%q,"state":"available","load":0,"future":null}`, NodeID{3}.String(), fp.String())
	valid := fmt.Sprintf(`{"session_id":%q,"version":1,"state":"open","node":%s,"operator_fp":%q,"operator_label":"","code":"01234567","code_expires_at":5,"expires_at":5,"future":[null]}`, id.String(), node, strings.ReplaceAll(fp.String(), "-", ""))
	for _, tc := range []struct{ name, from, to string }{
		{"extensions and ungrouped fingerprint", "", ""},
		{"explicit null", `"future":[null]`, `"ended_at":null`},
		{"missing required zero", `"load":0,`, ""},
		{"unrelated session", id.String(), SessionID{4}.String()},
		{"malformed ID", id.String(), "123"},
		{"wrong type", `"version":1`, `"version":"1"`},
		{"noncanonical code", "01234567", "0123-4567"},
		{"non-ASCII code", "01234567", "０1234567"},
		{"invalid fingerprint", fp.String(), "invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			c := testClient(t, "operator", func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/meta") {
					fmt.Fprint(w, validMeta)
					return
				}
				calls.Add(1)
				fmt.Fprint(w, strings.Replace(valid, tc.from, tc.to, 1))
			})
			s, err := c.Session(t.Context(), id, 0, 0, true)
			if tc.from == "" {
				if err != nil || s.OperatorFP != fp || s.Node.HubFP != fp {
					t.Fatal(s, err)
				}
			} else if err == nil {
				t.Fatal("accepted malformed session")
			}
			if calls.Load() != 1 {
				t.Fatal("retried response")
			}
		})
	}
}

func TestTicketBindingAndSize(t *testing.T) {
	id := SessionID{1}
	peer := identity.Fingerprint{2}
	node := fmt.Sprintf(`{"node_id":%q,"region":"west","addr":"127.0.0.1","port":9000,"hub_fp":%q,"state":"available","load":0}`, NodeID{3}.String(), identity.Fingerprint{3}.String())
	session := fmt.Sprintf(`{"session_id":%q,"version":2,"state":"authorized","node":%s,"operator_fp":%q,"operator_label":"","client_fp":%q,"redeemed_at":100,"pair_deadline":200,"expires_at":200}`, id.String(), node, identity.Fingerprint{1}.String(), peer.String())
	valid := fmt.Sprintf(`{"ticket":"_w","ticket_expires_at":150,"role":"wait","peer_fp":%q,"node":%s}`, peer.String(), node)
	for _, tc := range []struct {
		name, from, to string
		ok             bool
	}{
		{"opaque byte", "", "", true},
		{"limit", "_w", wire64.EncodeToString(make([]byte, 1024)), true},
		{"oversize", "_w", wire64.EncodeToString(make([]byte, 1025)), false},
		{"empty", "_w", "", false},
		{"padding", "_w", "_w==", false},
		{"other role", "wait", "dial", false},
		{"other peer", peer.String(), identity.Fingerprint{4}.String(), false},
		{"other node", NodeID{3}.String(), NodeID{4}.String(), false},
		{"other address", "127.0.0.1", "127.0.0.2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, "operator", func(w http.ResponseWriter, r *http.Request) {
				switch {
				case strings.HasSuffix(r.URL.Path, "/meta"):
					fmt.Fprint(w, validMeta)
				case strings.HasSuffix(r.URL.Path, "/tickets"):
					fmt.Fprint(w, strings.Replace(valid, tc.from, tc.to, 1))
				default:
					fmt.Fprint(w, session)
				}
			})
			got, err := c.Ticket(t.Context(), NewIdempotencyKey(), id)
			if (err == nil) != tc.ok || (!tc.ok && got.Raw != nil) {
				t.Fatalf("ticket validation: %v", err)
			}
		})
	}
}
