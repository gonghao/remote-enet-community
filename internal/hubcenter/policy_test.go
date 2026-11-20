package hubcenter

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

func TestAdmissionVectors(t *testing.T) {
	var vectors struct {
		Cases []struct {
			Name  string
			Input struct {
				Token  string
				Config struct {
					Center string `json:"center_id"`
					Node   string `json:"node_id"`
					Hub    string
					Gen    uint32
					Keys   []struct {
						Public string `json:"public_key"`
					}
				}
				TLSDevice string `json:"tls_device"`
				Route     struct {
					Role   hub.Role
					Target string
				}
				Now int64
			}
			Expect struct{ Result, Reason string }
		}
	}
	data, err := os.ReadFile("../../testdata/conformance/v08-ticket-reject.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if len(vectors.Cases) == 0 {
		t.Fatal("missing cases")
	}
	for _, v := range vectors.Cases {
		t.Run(v.Name, func(t *testing.T) {
			decode := func(s string) []byte {
				t.Helper()
				b, err := hex.DecodeString(s)
				if err != nil {
					t.Fatal(err)
				}
				return b
			}
			i := v.Input
			c := testConfig()
			c.Center.ID = i.Config.Center
			c.NodeID = [16]byte(decode(i.Config.Node))
			c.TicketKeys = nil
			for _, k := range i.Config.Keys {
				c.TicketKeys = append(c.TicketKeys, decode(k.Public))
			}
			p, err := NewPolicy(c, identity.Fingerprint(decode(i.Config.Hub)))
			if err != nil {
				t.Fatal(err)
			}
			p.SetGeneration(i.Config.Gen)
			_, err = p.Admit(identity.Fingerprint(decode(i.TLSDevice)), hub.Request{Role: i.Route.Role, Target: identity.Fingerprint(decode(i.Route.Target)), Token: decode(i.Token)}, time.Unix(i.Now, 0))
			if v.Expect.Result == "accept" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			var step *ticket.StepError
			if !errors.As(err, &step) || step.Step != v.Expect.Reason {
				t.Fatalf("got %v, want %s", err, v.Expect.Reason)
			}
		})
	}
}

func TestActivationRotationAndRouting(t *testing.T) {
	local := func() *identity.Local {
		t.Helper()
		id, err := identity.Generate()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	h, op, client, signer := local(), local(), local(), local()
	now := time.Now().Truncate(time.Second)
	cfg := testConfig()
	cfg.TicketKeys = []ed25519.PublicKey{signer.Key.Public().(ed25519.PublicKey)}
	p, err := NewPolicy(cfg, h.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	signed := func(role hub.Role, dev, peer *identity.Local, sid, jti byte) hub.Request {
		t.Helper()
		c := ticket.CenterClaims{Center: cfg.Center.ID, Node: cfg.NodeID, Hub: h.Fingerprint(), Generation: 42, Device: dev.Fingerprint(), Peer: peer.Fingerprint(), Role: uint8(role), Session: [16]byte{sid}, JTI: [16]byte{jti}, IssuedAt: now, NotBefore: now}
		token, err := ticket.SignCenter(signer.Key, c, ticket.CenterIssuePolicy{TicketTTL: 120, TicketMaxTTL: 300, ClockSkew: 30, HardDeadline: now.Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		return hub.Request{Role: role, Target: c.Peer, Token: token}
	}
	wait := signed(hub.RoleWait, op, client, 1, 2)
	dial := signed(hub.RoleDial, client, op, 1, 3)
	if _, err := p.Admit(op.Fingerprint(), wait, now); err == nil {
		t.Fatal("inactive admission")
	}
	p.SetGeneration(42)
	if _, err := p.Admit(client.Fingerprint(), signed(hub.RoleDial, client, h, 1, 4), now); err == nil {
		t.Fatal("accepted self dial")
	}
	if _, err := p.Admit(op.Fingerprint(), signed(hub.RoleWait, op, client, 0, 2), now); err == nil {
		t.Fatal("zero session")
	}
	g, err := p.Admit(op.Fingerprint(), wait, now)
	if err != nil || g.Session != (hub.SessionID{1}) || g.JTI != ([16]byte{2}) || !g.Expires.Equal(now.Add(150*time.Second)) || !p.PairingOperator().IsZero() {
		t.Fatalf("grant %+v: %v", g, err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	server := hub.NewServer(hub.Options{Local: h, Policy: p})
	server.SetAdmitting(true)
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("serve: %v", err)
		}
	}()
	connect := func(id *identity.Local, req hub.Request, want error) net.Conn {
		t.Helper()
		conn, err := hub.Connect(ctx, ln.Addr().String(), id, h.Fingerprint(), req)
		if !errors.Is(err, want) {
			t.Fatalf("connect: got %v, want %v", err, want)
		}
		if conn != nil {
			t.Cleanup(func() { conn.Close() })
		}
		return conn
	}
	w := connect(op, wait, nil)
	d := connect(client, dial, nil)
	connect(client, dial, hub.ErrUnauthorized)
	newKey := client.Key.Public().(ed25519.PublicKey)
	newKid := ticket.KidOf(newKey)
	if err := p.SetKeys([]ed25519.PublicKey{newKey, newKey}); err == nil {
		t.Fatal("accepted duplicate kid")
	}
	if p.Kids()[0] != ticket.KidOf(cfg.TicketKeys[0]) {
		t.Fatal("failed rotation replaced keys")
	}
	if err := p.SetKeys([]ed25519.PublicKey{newKey}); err != nil {
		t.Fatal(err)
	}
	if kids := p.Kids(); len(kids) != 1 || kids[0] != newKid {
		t.Fatal("incorrect kids")
	}
	var step *ticket.StepError
	if _, err := p.Admit(op.Fingerprint(), wait, now); !errors.As(err, &step) || step.Step != "8.4/3" {
		t.Fatalf("retired key: %v", err)
	}
	if !p.Valid(g, op.Fingerprint()) {
		t.Fatal("rotation revoked grant")
	}
	if err := w.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Write([]byte{42}); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := io.ReadFull(w, b[:]); err != nil || b[0] != 42 {
		t.Fatalf("relay: %x, %v", b, err)
	}
}
