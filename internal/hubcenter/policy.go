package hubcenter

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type keySet struct {
	keys map[ticket.Kid]ed25519.PublicKey
	kids []ticket.Kid
}

func newKeySet(keys []ed25519.PublicKey) (*keySet, error) {
	if len(keys) < 1 || len(keys) > 16 {
		return nil, errors.New("ticket_keys: expected 1..16 entries")
	}
	set := &keySet{keys: make(map[ticket.Kid]ed25519.PublicKey, len(keys))}
	for i, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("ticket_keys[%d]: expected Ed25519 public key", i)
		}
		kid := ticket.KidOf(key)
		if _, ok := set.keys[kid]; ok {
			return nil, fmt.Errorf("ticket_keys[%d]: duplicate kid", i)
		}
		set.keys[kid] = slices.Clone(key)
		set.kids = append(set.kids, kid)
	}
	return set, nil
}

// Policy verifies center tickets against immutable snapshots of the key list.
type Policy struct {
	expect     ticket.CenterExpect
	generation atomic.Uint32
	keys       atomic.Pointer[keySet]
}

var _ hub.Policy = (*Policy)(nil)

// NewPolicy leaves admission disabled until activation supplies a generation.
func NewPolicy(cfg Config, hubFP identity.Fingerprint) (*Policy, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	p := &Policy{expect: ticket.CenterExpect{Center: cfg.Center.ID, Node: cfg.NodeID, Hub: hubFP, ClockSkew: int64(ClockSkew / time.Second), TicketMaxTTL: int64(TicketMaxTTL / time.Second)}}
	if err := p.SetKeys(cfg.TicketKeys); err != nil {
		return nil, err
	}
	return p, nil
}

// SetGeneration installs the activated generation. Zero disables admission.
func (p *Policy) SetGeneration(gen uint32) { p.generation.Store(gen) }

// SetKeys publishes the complete list only after validating every key.
func (p *Policy) SetKeys(keys []ed25519.PublicKey) error {
	set, err := newKeySet(keys)
	if err != nil {
		return err
	}
	p.keys.Store(set)
	return nil
}

// Kids returns an independent copy in configuration order for node heartbeats.
func (p *Policy) Kids() []ticket.Kid { return slices.Clone(p.keys.Load().kids) }

// Admit verifies the ticket outside the route lock. The server commits its JTI.
func (p *Policy) Admit(dev identity.Fingerprint, req hub.Request, now time.Time) (hub.Grant, error) {
	e := p.expect
	e.Generation = p.generation.Load()
	if e.Generation == 0 {
		return hub.Grant{}, errors.New("node is not activated")
	}
	e.TLSDevice, e.Target, e.Role = dev, req.Target, uint8(req.Role)
	set := p.keys.Load()
	c, err := ticket.VerifyCenter(req.Token, func(k ticket.Kid) (ed25519.PublicKey, bool) { v, ok := set.keys[k]; return v, ok }, e, now)
	if err != nil {
		return hub.Grant{}, err
	}
	if req.Role == hub.RoleDial && req.Target == e.Hub {
		return hub.Grant{}, errors.New("center pairing entry disabled")
	}
	if c.Session == ([16]byte{}) {
		return hub.Grant{}, errors.New("zero center session ID")
	}
	return hub.Grant{Session: c.Session, JTI: c.JTI, Expires: c.Expires.Add(ClockSkew)}, nil
}

// Valid preserves established sessions across key rotation. Heartbeats revoke sessions.
func (*Policy) Valid(hub.Grant, identity.Fingerprint) bool { return true }

// PairingOperator disables the standalone bootstrap route.
func (*Policy) PairingOperator() identity.Fingerprint { return identity.Fingerprint{} }
