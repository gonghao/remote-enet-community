package ticket

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"math"
	"regexp"
	"time"

	"remote-enet/internal/identity"
)

var centerID = regexp.MustCompile("^[a-z0-9.-]{1,32}$")

// CenterClaims contains the required v1 fields. Times have Unix-second precision.
type CenterClaims struct {
	Center                       string
	Account, Session, Node, JTI  [16]byte
	Device, Peer, Hub            identity.Fingerprint
	Role                         uint8
	IssuedAt, NotBefore, Expires time.Time
	Generation                   uint32
}

// CenterExpect binds the node's active configuration to one TLS connection and route.
// ClockSkew and TicketMaxTTL are in seconds.
type CenterExpect struct {
	Center                  string
	Node                    [16]byte
	Hub                     identity.Fingerprint
	Generation              uint32
	TLSDevice, Target       identity.Fingerprint
	Role                    uint8
	ClockSkew, TicketMaxTTL int64
}

// CenterIssuePolicy specifies durations in seconds and the earliest session or account deadline.
type CenterIssuePolicy struct {
	TicketTTL, TicketMaxTTL, ClockSkew int64
	HardDeadline                       time.Time
}

// SignCenter derives expiration from the policy. The Expires field is output-only.
func SignCenter(priv ed25519.PrivateKey, c CenterClaims, policy CenterIssuePolicy) ([]byte, error) {
	if !centerID.MatchString(c.Center) || (c.Role != roleDial && c.Role != roleWait) || c.Peer.IsZero() {
		return nil, errors.New("invalid center, role or peer")
	}
	i, n := c.IssuedAt.Unix(), c.NotBefore.Unix()
	deadline := policy.HardDeadline.Unix()
	if i < 0 || i > math.MaxUint32 || policy.TicketTTL <= 0 || deadline <= i || policy.ClockSkew < 0 || policy.ClockSkew > (deadline-i)/2 {
		return nil, errors.New("invalid issuance window")
	}
	e := i + min(policy.TicketTTL, deadline-i-2*policy.ClockSkew)
	if i != n {
		return nil, errors.New("signing requires iat equal to nbf")
	}
	if err := checkWindow(i, n, e, i, 0, policy.TicketMaxTTL); err != nil {
		return nil, err
	}
	return signFields(priv, map[uint64]any{
		1: 1, 2: c.Center, 3: c.Account[:], 4: c.Device[:], 5: c.Role, 6: c.Peer[:],
		7: c.Session[:], 8: c.Node[:], 9: c.Hub[:], 10: i, 11: n, 12: e, 13: c.JTI[:], 14: c.Generation,
	})
}

// VerifyCenter applies CENTER-CONTRACT §8.4 steps 1–9 before acquiring the route lock.
// Failures carry the contract step in a StepError.
func VerifyCenter(raw []byte, keys func(Kid) (ed25519.PublicKey, bool), expect CenterExpect, now time.Time) (CenterClaims, error) {
	if len(raw) == 0 {
		return CenterClaims{}, &StepError{"8.4/1", errors.New("empty token")}
	}
	_, payload, step, err := openEnvelope(raw, keys, false)
	if err != nil {
		return CenterClaims{}, &StepError{fmt.Sprintf("8.4/%d", step), err}
	}
	f, err := decodeFields(payload, 1)
	if err != nil {
		return CenterClaims{}, &StepError{"8.4/5", err}
	}
	c := CenterClaims{
		Center:     f[2].(string),
		Account:    [16]byte(f[3].([]byte)),
		Device:     identity.Fingerprint(f[4].([]byte)),
		Role:       uint8(f[5].(uint64)),
		Peer:       identity.Fingerprint(f[6].([]byte)),
		Session:    [16]byte(f[7].([]byte)),
		Node:       [16]byte(f[8].([]byte)),
		Hub:        identity.Fingerprint(f[9].([]byte)),
		IssuedAt:   claimTime(f[10]),
		NotBefore:  claimTime(f[11]),
		Expires:    claimTime(f[12]),
		JTI:        [16]byte(f[13].([]byte)),
		Generation: uint32(f[14].(uint64)),
	}
	if c.Center != expect.Center || c.Node != expect.Node || c.Hub != expect.Hub || c.Generation != expect.Generation {
		return CenterClaims{}, &StepError{"8.4/6", errors.New("wrong center, node, hub or generation")}
	}
	if c.Device != expect.TLSDevice {
		return CenterClaims{}, &StepError{"8.4/7", errors.New("TLS device mismatch")}
	}
	if c.Role != expect.Role || expect.Target.IsZero() || c.Peer != expect.Target {
		return CenterClaims{}, &StepError{"8.4/8", errors.New("invalid route binding")}
	}
	if err := checkWindow(c.IssuedAt.Unix(), c.NotBefore.Unix(), c.Expires.Unix(), now.Unix(), expect.ClockSkew, expect.TicketMaxTTL); err != nil {
		return CenterClaims{}, &StepError{"8.4/9", err}
	}
	return c, nil
}
