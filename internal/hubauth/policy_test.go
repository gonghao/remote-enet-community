package hubauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/fxamacker/cbor/v2"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type fixture struct {
	cfg           Config
	key           ed25519.PrivateKey
	h, op, client *identity.Local
	now           time.Time
}

func local(t *testing.T) *identity.Local {
	t.Helper()
	v, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func setup(t *testing.T) fixture {
	t.Helper()
	f := fixture{cfg: defaults(), key: ed25519.NewKeyFromSeed(make([]byte, 32)), h: local(t), op: local(t), client: local(t), now: time.Unix(1700000000, 0)}
	c := &f.cfg
	c.Version = 1
	c.Mode = "selfhost"
	c.Listen = ":443"
	c.IdentityDir = t.TempDir()
	if err := f.h.Save(c.IdentityDir); err != nil {
		t.Fatal(err)
	}
	c.PairingOperator = f.op.Fingerprint().String()
	c.Issuers = []Issuer{{Operator: c.PairingOperator, PublicKey: base64.RawURLEncoding.EncodeToString(f.key.Public().(ed25519.PublicKey))}}
	return f
}

func (f fixture) claims(role hub.Role) ticket.Claims {
	c := ticket.Claims{Role: uint8(role), Hub: f.h.Fingerprint(), IssuedAt: f.now, NotBefore: f.now, Expires: f.now.Add(time.Hour), Device: f.op.Fingerprint()}
	if role == hub.RoleDial {
		c.Device = f.client.Fingerprint()
		c.Peer = f.op.Fingerprint()
	}
	return c
}

func (f fixture) sign(t *testing.T, c ticket.Claims) []byte {
	t.Helper()
	b, err := ticket.Sign(f.key, c)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (f fixture) policy(t *testing.T) *Policy {
	t.Helper()
	p, err := NewPolicy(f.cfg, f.h.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Start with a genuine ticket, then re-sign fields that the normal signer refuses.
func (f fixture) edit(t *testing.T, raw []byte, key uint64, value any) []byte {
	t.Helper()
	enc, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		t.Fatal(err)
	}
	marshal := func(v any) []byte {
		b, err := enc.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	decode := func(b []byte, v any) {
		if err := cbor.Unmarshal(b, v); err != nil {
			t.Fatal(err)
		}
	}
	var tag cbor.RawTag
	decode(raw, &tag)
	var parts []any
	decode(tag.Content, &parts)
	var fields map[uint64]any
	decode(parts[2].([]byte), &fields)
	fields[key] = value
	parts[2] = marshal(fields)
	parts[3] = ed25519.Sign(f.key, marshal([]any{"Signature1", parts[0], []byte{}, parts[2]}))
	return marshal(cbor.Tag{Number: 18, Content: parts})
}

func TestAdmission(t *testing.T) {
	f := setup(t)
	h, op, dev := f.h.Fingerprint(), f.op.Fingerprint(), f.client.Fingerprint()
	wait, dial := f.claims(hub.RoleWait), f.claims(hub.RoleDial)
	type sample struct {
		claims ticket.Claims
		req    hub.Request
		dev    identity.Fingerprint
		now    time.Time
		cfg    Config
	}
	cases := []struct {
		name, step string
		change     func(*sample)
		wait       bool
	}{
		{"dial", "", nil, false},
		{"wait", "", nil, true},
		{"hub", "A6", func(s *sample) { s.claims.Hub = dev }, false},
		{"device", "A7", func(s *sample) { s.dev = h }, false},
		{"role", "A8", func(s *sample) { s.req.Role = hub.RoleWait }, false},
		{"target", "A8", func(s *sample) { s.req.Target = dev }, false},
		{"wait target", "A8", func(s *sample) { s.req.Target = dev }, true},
		{"wait scope", "A9", func(s *sample) { s.dev = dev; s.claims.Device = dev }, true},
		{"dial scope", "A9", func(s *sample) { s.req.Target = dev; s.claims.Peer = dev }, false},
		{"wrong pairing operator", "A9", func(s *sample) {
			s.cfg.PairingOperator = dev.String()
			s.cfg.Issuers = append(s.cfg.Issuers, Issuer{Operator: dev.String(), PublicKey: base64.RawURLEncoding.EncodeToString(f.client.Key.Public().(ed25519.PublicKey))})
			s.claims.PairUntil = f.now
		}, true},
		{"exp plus skew minus one", "", func(s *sample) { s.now = s.claims.Expires.Add(29 * time.Second) }, false},
		{"exp plus skew", "", func(s *sample) { s.now = s.claims.Expires.Add(30 * time.Second) }, false},
		{"expired", "A10", func(s *sample) { s.now = s.claims.Expires.Add(31 * time.Second) }, false},
		{"iat ahead", "A10", func(s *sample) { s.now = f.now.Add(-31 * time.Second) }, false},
		{"long ticket", "A10", func(s *sample) { s.claims.Expires = f.now.Add(2678401 * time.Second) }, false},
		{"long bearer", "A10", func(s *sample) {
			s.claims.Bearer = true
			s.claims.Device = identity.Fingerprint{}
			s.claims.Expires = f.now.Add(86401 * time.Second)
		}, false},
		{"long window", "A10", func(s *sample) { s.claims.PairUntil = f.now.Add(901 * time.Second) }, true},
		{"uint32 limit", "", func(s *sample) {
			s.claims.Expires = time.Unix(math.MaxUint32, 0)
			s.claims.IssuedAt = s.claims.Expires.Add(-time.Hour)
			s.claims.NotBefore = s.claims.IssuedAt
			s.now = s.claims.Expires.Add(30 * time.Second)
		}, false},
		{"deny device", "A11", func(s *sample) { s.cfg.Deny.Devices = []string{dev.String()} }, false},
		{"deny bearer device", "A11", func(s *sample) {
			s.claims.Bearer = true
			s.claims.Device = identity.Fingerprint{}
			s.cfg.Deny.Devices = []string{dev.String()}
		}, false},
		{"deny jti", "A11", func(s *sample) { s.cfg.Deny.JTIs = []string{base64.RawURLEncoding.EncodeToString(s.claims.JTI[:])} }, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := dial
			if tc.wait {
				c = wait
			}
			s := sample{c, hub.Request{Role: hub.Role(c.Role), Target: c.Peer}, c.Device, f.now, f.cfg}
			if tc.change != nil {
				tc.change(&s)
			}
			raw := f.sign(t, s.claims)
			s.req.Token = raw
			p, err := NewPolicy(s.cfg, h)
			if err != nil {
				t.Fatal(err)
			}
			_, err = p.Admit(s.dev, s.req, s.now)
			checkStep(t, err, tc.step)
			want := tc.step
			if want == "A8" {
				want = ""
			}
			checkStep(t, Explain(s.cfg, h, raw, s.dev, s.now), want)
		})
	}
	p := f.policy(t)
	raw := f.sign(t, dial)
	badSig := append([]byte(nil), raw...)
	badSig[len(badSig)-1] ^= 1
	other := f
	other.key = f.client.Key
	for _, tc := range []struct {
		step string
		raw  []byte
	}{
		{"A1", nil},
		{"A2", []byte{1}},
		{"A3", other.sign(t, dial)},
		{"A4", badSig},
		{"A5", f.edit(t, raw, 1, 3)},
		{"A10", f.edit(t, raw, 11, f.now.Unix()+31)},
		{"A10", f.edit(t, f.sign(t, wait), 15, wait.Expires.Unix()+1)},
	} {
		role, target, device := hub.RoleDial, op, dev
		if tc.raw != nil && tc.step == "A10" {
			_, claims, _ := ticket.Inspect(tc.raw)
			role, target, device = hub.Role(claims.Role), claims.Peer, claims.Device
		}
		_, err := p.Admit(device, hub.Request{Role: role, Target: target, Token: tc.raw}, f.now)
		checkStep(t, err, tc.step)
		want := tc.step
		if want == "A1" {
			want = "A2"
		}
		checkStep(t, Explain(f.cfg, h, tc.raw, device, f.now), want)
	}
}

func checkStep(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if err == nil || !strings.HasPrefix(err.Error(), want+":") {
		t.Fatalf("expected %s, got %v", want, err)
	}
}

func TestPairingAndRevocation(t *testing.T) {
	f := setup(t)
	dev := f.client.Fingerprint()
	req := hub.Request{Role: hub.RoleDial, Target: f.h.Fingerprint()}
	p := f.policy(t)
	entry, err := p.Admit(dev, req, f.now)
	checkStep(t, err, "")
	c := f.claims(hub.RoleWait)
	c.PairUntil = f.now.Add(time.Minute)
	wait, err := p.Admit(c.Device, hub.Request{Role: hub.RoleWait, Token: f.sign(t, c)}, f.now)
	checkStep(t, err, "")
	if !entry.Expires.IsZero() || !wait.Expires.Equal(c.Expires.Add(30*time.Second)) || !wait.PairingUntil.Equal(c.PairUntil.Add(30*time.Second)) {
		t.Fatal("incorrect deadlines")
	}
	if !p.Valid(entry, dev) || !p.Valid(wait, c.Device) || p.Valid(wait, dev) {
		t.Fatal("incorrect device binding")
	}
	req.Token = []byte{1}
	_, err = p.Admit(dev, req, f.now)
	checkStep(t, err, "A0")
	req.Token = nil
	f.cfg.Deny.Devices = []string{dev.String()}
	p = f.policy(t)
	_, err = p.Admit(dev, req, f.now)
	checkStep(t, err, "A0")
	if p.Valid(entry, dev) {
		t.Fatal("revoked entry")
	}
	f.cfg.Deny = Deny{}
	f.cfg.PairingOperator = ""
	p = f.policy(t)
	_, err = p.Admit(dev, req, f.now)
	checkStep(t, err, "A0")
	if p.Valid(entry, dev) || p.Valid(wait, c.Device) {
		t.Fatal("disabled pairing operator")
	}
	f.cfg.PairingOperator = c.Device.String()
	f.cfg.Deny.JTIs = []string{base64.RawURLEncoding.EncodeToString(c.JTI[:])}
	if f.policy(t).Valid(wait, c.Device) {
		t.Fatal("revoked jti")
	}
	f.cfg.Deny = Deny{}
	f.cfg.Issuers[0].PublicKey = base64.RawURLEncoding.EncodeToString(f.client.Key.Public().(ed25519.PublicKey))
	if f.policy(t).Valid(wait, c.Device) {
		t.Fatal("revoked issuer")
	}
}

func TestReloadIssuerScope(t *testing.T) {
	f := setup(t)
	c := f.claims(hub.RoleDial)
	req := hub.Request{Role: hub.RoleDial, Target: c.Peer, Token: f.sign(t, c)}
	original := f.policy(t)
	g, err := original.Admit(c.Device, req, f.now)
	checkStep(t, err, "")
	// The kid stays trusted, but it no longer represents the same Operator.
	f.cfg.Issuers[0].Operator = f.client.Fingerprint().String()
	f.cfg.PairingOperator = ""
	if f.policy(t).Valid(g, c.Device) || !original.Valid(g, c.Device) {
		t.Fatal("grant survived issuer reassignment or original snapshot changed")
	}
}
