package session

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/ticket"
	"remote-enet/internal/trust"
)

func deviceClaims() ticket.Claims {
	now := time.Unix(1000, 0)
	return ticket.Claims{Role: 1, Device: identity.Fingerprint{1}, Peer: identity.Fingerprint{2}, Hub: identity.Fingerprint{3}, IssuedAt: now, NotBefore: now, Expires: now.Add(time.Hour)}
}

func signedTicket(t *testing.T, c ticket.Claims) []byte {
	t.Helper()
	raw, err := ticket.Sign(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)), c)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestHubTicketExchange(t *testing.T) {
	for _, path := range []string{"trusted", "invite", "digital", "grant failure", "digital grant failure", "endpoint mismatch"} {
		t.Run(path, func(t *testing.T) {
			ctx := testContext(t)
			claims := deviceClaims()
			raw := signedTicket(t, claims)
			a, b := memtransport.Pair(t, claims.Device, claims.Peer)
			c := Options{RequireHubTicket: true, Initiator: true, Role: RoleClient, LocalFP: claims.Device, EndpointHub: claims.Hub, Now: func() time.Time { return claims.IssuedAt }}
			o := Options{Role: RoleOperator, LocalFP: claims.Peer, GrantTicket: func(client identity.Fingerprint) ([]byte, error) {
				if client != claims.Device {
					t.Error("wrong grant recipient")
				}
				if path == "grant failure" || path == "digital grant failure" {
					return nil, errors.New("private issuer detail")
				}
				return raw, nil
			}}
			if path == "endpoint mismatch" {
				c.EndpointHub[0]++
			}
			switch path {
			case "trusted", "grant failure", "endpoint mismatch":
				o.Trusted = func(identity.Fingerprint) bool { return true }
			case "invite":
				c.Token = [16]byte{7}
				o.AcceptToken = func(_ identity.Fingerprint, token [16]byte) bool { return token == c.Token }
			case "digital", "digital grant failure":
				c.HubFP, o.HubFP = claims.Hub, claims.Hub
				c.DigitalCode = "00001234"
				o.DigitalManager = testResponder{code: c.DigitalCode}
				o.Remember = func(context.Context, identity.Fingerprint) error {
					if path == "digital grant failure" {
						t.Error("committed trust after issuer failure")
					}
					return nil
				}
			}
			op := make(chan error, 1)
			go func() { _, err := Run(ctx, exportSession{b, [32]byte{1}}, o); op <- err }()
			client, err := Run(ctx, exportSession{a, [32]byte{1}}, c)
			opErr := <-op
			if path == "endpoint mismatch" {
				if client != nil || !errors.Is(err, ReasonHubTicket) {
					t.Fatalf("endpoint mismatch: %v", err)
				}
				return
			}
			if path == "grant failure" || path == "digital grant failure" {
				if client != nil || !errors.Is(err, ReasonHubTicket) || !errors.Is(opErr, ReasonHubTicket) {
					t.Fatalf("grant failure: %v, %v", err, opErr)
				}
				return
			}
			if err != nil || opErr != nil {
				t.Fatalf("handshake: %v, %v", err, opErr)
			}
			got := client.HubTicket()
			if !bytes.Equal(got, raw) {
				t.Fatal("wrong device ticket")
			}
			got[0]++
			if !bytes.Equal(client.HubTicket(), raw) {
				t.Fatal("ticket aliases session state")
			}
		})
	}
}

func TestClientRejectsHubTicket(t *testing.T) {
	for _, name := range []string{"missing", "oversize", "malformed", "device", "peer", "hub", "bearer", "wait", "expired", "expires now", "bearer invite with old operator", "hub code with old operator"} {
		t.Run(name, func(t *testing.T) {
			claims := deviceClaims()
			claims.Peer = identity.Fingerprint{1} // handshakeTransport peer
			now := claims.IssuedAt
			opts := Options{Role: RoleClient, Initiator: true, LocalFP: claims.Device, EndpointHub: claims.Hub, Now: func() time.Time { return now }}
			h := proto.Hello{ProtoVersion: proto.Version, Role: "operator", Capabilities: []string{proto.HubTicketCapability}}
			var frame []byte
			want := ReasonHubTicket
			switch name {
			case "missing":
			case "oversize":
				frame = binary.BigEndian.AppendUint32(nil, 1101)
			case "malformed":
				frame = encodedMessages(t, proto.HubTicket{Ticket: []byte{1}})
			case "bearer invite with old operator":
				h.Capabilities = nil
				opts.RequireHubTicket = true
			case "hub code with old operator":
				h.Capabilities = nil
				opts.HubFP = claims.Hub
				opts.DigitalCode = "00001234"
				want = ReasonUnsupported
			default:
				switch name {
				case "device":
					claims.Device[0]++
				case "peer":
					claims.Peer[0]++
				case "hub":
					claims.Hub[0]++
				case "bearer":
					claims.Device = identity.Fingerprint{}
					claims.Bearer = true
				case "wait":
					claims.Role = 2
					claims.Peer = identity.Fingerprint{}
				case "expired":
					now = claims.Expires.Add(time.Second)
				case "expires now":
					now = claims.Expires
				}
				frame = encodedMessages(t, proto.HubTicket{Ticket: signedTicket(t, claims)})
			}
			input := append(encodedMessages(t, h, proto.PairResult{Accepted: true}), frame...)
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(input)}
			s, err := Run(testContext(t), ts, opts)
			if s != nil || !errors.Is(err, want) || ts.Err() == nil {
				t.Fatalf("handshake: %v, transport: %v", err, ts.Err())
			}
		})
	}
}

func TestOldClientGetsNoHubTicket(t *testing.T) {
	ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
	ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t, proto.Hello{ProtoVersion: proto.Version, Role: "client"}))}
	record := &recordingTransport{Session: ts}
	s := Session{ts: record, opts: Options{Initiator: true, Role: RoleOperator, Capabilities: []string{proto.HubTicketCapability}, Trusted: func(identity.Fingerprint) bool { return true }, GrantTicket: func(identity.Fingerprint) ([]byte, error) { t.Fatal("unexpected grant"); return nil, nil }}}
	if err := s.handshake(testContext(t)); err != nil {
		t.Fatal(err)
	}
	want := encodedMessages(t, proto.Hello{ProtoVersion: proto.Version, Role: "operator", Capabilities: s.opts.Capabilities}, proto.PairResult{Accepted: true})
	if len(s.NegotiatedCapabilities) != 0 || !bytes.Equal(record.output.Bytes(), want) {
		t.Fatal("unexpected negotiated capability or frame")
	}
}

func TestInvitationSurvivesIssuerFailure(t *testing.T) {
	ctx := testContext(t)
	store, err := trust.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.Issue(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	claims := deviceClaims()
	raw := signedTicket(t, claims)
	for _, fail := range []bool{true, false} {
		a, b := memtransport.Pair(t, claims.Device, claims.Peer)
		o := Options{Role: RoleOperator, AcceptToken: func(fp identity.Fingerprint, v [16]byte) bool {
			ok, err := store.Consume(ctx, fp, v)
			if err != nil {
				t.Error(err)
			}
			return ok
		}, GrantTicket: func(identity.Fingerprint) ([]byte, error) {
			if fail {
				return nil, errors.New("issuer unavailable")
			}
			return raw, nil
		}}
		op := make(chan error, 1)
		go func() { _, err := Run(ctx, b, o); op <- err }()
		client, err := Run(ctx, a, Options{Role: RoleClient, Initiator: true, Token: token, LocalFP: claims.Device, EndpointHub: claims.Hub, RequireHubTicket: true, Now: func() time.Time { return claims.IssuedAt }})
		opErr := <-op
		trusted, storeErr := store.Trusted(ctx, claims.Device)
		if storeErr != nil || trusted == fail {
			t.Fatalf("trust after issuer failure=%t: %t, %v", fail, trusted, storeErr)
		}
		if fail {
			if !errors.Is(err, ReasonHubTicket) || !errors.Is(opErr, ReasonHubTicket) {
				t.Fatalf("issuer refusal: %v, %v", err, opErr)
			}
		} else if err != nil || opErr != nil || !bytes.Equal(client.HubTicket(), raw) {
			t.Fatalf("retry: %v, %v", err, opErr)
		}
	}
}
