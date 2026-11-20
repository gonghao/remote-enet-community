package hubauth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type binding struct {
	pub      ed25519.PublicKey
	operator identity.Fingerprint
}

// Policy is an immutable snapshot shared by admission and locked revocation checks.
type Policy struct {
	hub, operator                      identity.Fingerprint
	issuers                            map[ticket.Kid]binding
	devices                            map[identity.Fingerprint]bool
	jtis                               map[[16]byte]bool
	skew, maxTTL, bootstrapTTL, window int64
}

var _ hub.Policy = (*Policy)(nil)

type reference struct {
	kid                    ticket.Kid
	jti                    [16]byte
	dev, operator          identity.Fingerprint
	entry, requiresPairing bool
}

// NewPolicy copies all trust and revocation data out of cfg.
func NewPolicy(cfg Config, hubFP identity.Fingerprint) (*Policy, error) {
	p, err := cfg.compile()
	if err != nil {
		return nil, err
	}
	p.hub = hubFP
	return p, nil
}

// PairingOperator returns zero when the standalone entry is disabled.
func (p *Policy) PairingOperator() identity.Fingerprint { return p.operator }

// Admit authenticates the outer request before the Hub takes its routing lock.
func (p *Policy) Admit(dev identity.Fingerprint, req hub.Request, now time.Time) (hub.Grant, error) {
	if req.Role == hub.RoleDial && req.Target == p.hub {
		if len(req.Token) != 0 || p.operator.IsZero() || p.devices[dev] {
			return hub.Grant{}, errors.New("A0: pairing entry denied")
		}
		return hub.Grant{Ref: reference{dev: dev, operator: p.operator, entry: true, requiresPairing: true}}, nil
	}
	if len(req.Token) == 0 {
		return hub.Grant{}, errors.New("A1: missing ticket")
	}
	return p.verify(req.Token, dev, &req, now)
}

func (p *Policy) verify(raw []byte, dev identity.Fingerprint, req *hub.Request, now time.Time) (hub.Grant, error) {
	kid, c, err := ticket.Verify(raw, func(k ticket.Kid) (ed25519.PublicKey, bool) { b, ok := p.issuers[k]; return b.pub, ok })
	if err != nil {
		return hub.Grant{}, err
	}
	if c.Hub != p.hub {
		return hub.Grant{}, errors.New("A6: incorrect hub")
	}
	if (!c.Bearer && c.Device != dev) || (c.Bearer && c.Role != uint8(hub.RoleDial)) {
		return hub.Grant{}, errors.New("A7: device binding")
	}
	if (c.Role == uint8(hub.RoleDial) && c.Peer.IsZero()) || (req != nil && (hub.Role(c.Role) != req.Role || c.Peer != req.Target)) {
		return hub.Grant{}, errors.New("A8: route mismatch")
	}
	op := p.issuers[kid].operator
	window := !c.PairUntil.IsZero()
	if (c.Role == uint8(hub.RoleWait) && dev != op) || (c.Role == uint8(hub.RoleDial) && c.Peer != op) || (window && op != p.operator) {
		return hub.Grant{}, errors.New("A9: issuer scope")
	}
	if err := ticket.CheckTime(c, now, p.skew, p.maxTTL, p.bootstrapTTL, p.window); err != nil {
		return hub.Grant{}, err
	}
	if p.jtis[c.JTI] || p.devices[dev] {
		return hub.Grant{}, errors.New("A11: revoked")
	}
	skew := time.Duration(p.skew) * time.Second
	g := hub.Grant{Ref: reference{kid: kid, jti: c.JTI, dev: dev, operator: op, requiresPairing: window}, Expires: c.Expires.Add(skew)}
	if window {
		g.PairingUntil = c.PairUntil.Add(skew)
	}
	return g, nil
}

// Valid rechecks the original issuer scope and revocations without checking time.
func (p *Policy) Valid(g hub.Grant, dev identity.Fingerprint) bool {
	r, ok := g.Ref.(reference)
	if !ok || r.dev != dev || p.devices[dev] || (r.requiresPairing && r.operator != p.operator) {
		return false
	}
	if r.entry {
		return true
	}
	b, ok := p.issuers[r.kid]
	return ok && b.operator == r.operator && !p.jtis[r.jti]
}

// Explain evaluates a ticket offline. Without an outer request A8 has only
// the signed role and peer, whose field profile Verify already checked.
func Explain(cfg Config, hubFP identity.Fingerprint, raw []byte, dev identity.Fingerprint, now time.Time) error {
	p, err := NewPolicy(cfg, hubFP)
	if err != nil {
		return err
	}
	_, err = p.verify(raw, dev, nil, now)
	return err
}
