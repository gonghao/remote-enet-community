package center

import (
	"fmt"

	"remote-enet/internal/identity"
)

// SessionID and the other fixed-size identifiers use unpadded base64url in JSON and in URL path segments.
type SessionID [16]byte

// NodeID is assigned at node registration and survives activation.
type NodeID [16]byte

// AccountID identifies the account independently of its login code.
type AccountID [16]byte

// KeyID is the first eight bytes of the ticket signing public key digest.
type KeyID [8]byte

func decodeFixed(text, dst []byte) error {
	b, err := wire64.DecodeString(string(text))
	if err != nil || len(b) != len(dst) || wire64.EncodeToString(b) != string(text) {
		return fmt.Errorf("center: invalid base64url identifier")
	}
	copy(dst, b)
	return nil
}
func (id SessionID) String() string                { return wire64.EncodeToString(id[:]) }
func (id SessionID) MarshalText() ([]byte, error)  { return []byte(id.String()), nil }
func (id *SessionID) UnmarshalText(b []byte) error { return decodeFixed(b, id[:]) }
func (id NodeID) String() string                   { return wire64.EncodeToString(id[:]) }
func (id NodeID) MarshalText() ([]byte, error)     { return []byte(id.String()), nil }
func (id *NodeID) UnmarshalText(b []byte) error    { return decodeFixed(b, id[:]) }
func (id AccountID) String() string                { return wire64.EncodeToString(id[:]) }
func (id AccountID) MarshalText() ([]byte, error)  { return []byte(id.String()), nil }
func (id *AccountID) UnmarshalText(b []byte) error { return decodeFixed(b, id[:]) }
func (id KeyID) String() string                    { return wire64.EncodeToString(id[:]) }
func (id KeyID) MarshalText() ([]byte, error)      { return []byte(id.String()), nil }
func (id *KeyID) UnmarshalText(b []byte) error     { return decodeFixed(b, id[:]) }

// Device contains the effective device expiry returned on login, in Unix seconds.
type Device struct {
	DeviceFP  identity.Fingerprint `json:"device_fp"`
	Label     string               `json:"label"`
	ExpiresAt int64                `json:"expires_at"`
}

// Account uses Unix seconds for timestamps and seconds for expiry grace.
type Account struct {
	AccountID    AccountID `json:"account_id"`
	Label        string    `json:"label"`
	Status       string    `json:"status"`
	ValidUntil   int64     `json:"valid_until"`
	ExpiryGraceS int64     `json:"expiry_grace_s"`
	Quota        int       `json:"quota"`
	QuotaInUse   int       `json:"quota_in_use"`
	ServerTime   int64     `json:"server_time"`
}

// AccountView includes only non-ended session summaries.
type AccountView struct {
	Account   Account          `json:"account"`
	ExpiresAt int64            `json:"expires_at"`
	Sessions  []SessionSummary `json:"sessions"`
}

// SessionSummary identifies the operator holding an account slot.
type SessionSummary struct {
	SessionID  SessionID            `json:"session_id"`
	State      string               `json:"state"`
	NodeID     NodeID               `json:"node_id"`
	OperatorFP identity.Fingerprint `json:"operator_fp"`
}

// DeviceEntry marks the requesting operator with Current.
type DeviceEntry struct {
	DeviceFP    identity.Fingerprint `json:"device_fp"`
	Label       string               `json:"label"`
	LastLoginAt int64                `json:"last_login_at"`
	LastUsedAt  int64                `json:"last_used_at"`
	Current     bool                 `json:"current"`
}

// Node reports allocation load as a percentage.
type Node struct {
	NodeID NodeID               `json:"node_id"`
	Region string               `json:"region"`
	Addr   string               `json:"addr"`
	Port   uint16               `json:"port"`
	HubFP  identity.Fingerprint `json:"hub_fp"`
	State  string               `json:"state"`
	Load   int                  `json:"load"`
}

// Session uses nil for fields omitted for this role or state.
type Session struct {
	SessionID     SessionID             `json:"session_id"`
	Version       int64                 `json:"version"`
	State         string                `json:"state"`
	Node          Node                  `json:"node"`
	Code          *string               `json:"code"`
	CodeExpiresAt *int64                `json:"code_expires_at"`
	ClientFP      *identity.Fingerprint `json:"client_fp"`
	RedeemedAt    *int64                `json:"redeemed_at"`
	PairDeadline  *int64                `json:"pair_deadline"`
	OperatorFP    identity.Fingerprint  `json:"operator_fp"`
	OperatorLabel string                `json:"operator_label"`
	ExpiresAt     *int64                `json:"expires_at"`
	EndReason     *string               `json:"end_reason"`
	EndedAt       *int64                `json:"ended_at"`
}

// Ticket contains opaque bytes bound to the peer and node.
type Ticket struct {
	Raw             ticketBytes          `json:"ticket"`
	TicketExpiresAt int64                `json:"ticket_expires_at"`
	Role            string               `json:"role"`
	PeerFP          identity.Fingerprint `json:"peer_fp"`
	Node            Node                 `json:"node"`
}

// Activation advertises locally configured verification keys.
type Activation struct {
	Version string  `json:"version"`
	Kids    []KeyID `json:"kids"`
}

// NodePolicy uses whole seconds for both policy intervals.
type NodePolicy struct {
	ServerTime         int64  `json:"server_time"`
	NodeState          string `json:"node_state"`
	HeartbeatIntervalS int64  `json:"heartbeat_interval_s"`
	LeaseTTLS          int64  `json:"lease_ttl_s"`
}

// Activated binds future heartbeats to the returned generation.
type Activated struct {
	Gen uint32 `json:"gen"`
	NodePolicy
}

// Heartbeat uses a strictly increasing Seq within Gen.
type Heartbeat struct {
	Gen      uint32         `json:"gen"`
	Seq      uint64         `json:"seq"`
	Version  string         `json:"version"`
	Kids     []KeyID        `json:"kids"`
	Load     Load           `json:"load"`
	Sessions []SessionUsage `json:"sessions"`
	Closed   []ClosedUsage  `json:"closed"`
}

// Load reports instantaneous connection counts and byte rates.
type Load struct {
	Conns    uint64 `json:"conns"`
	Sessions uint64 `json:"sessions"`
	Waiting  uint64 `json:"waiting"`
	RxBps    uint64 `json:"rx_bps"`
	TxBps    uint64 `json:"tx_bps"`
}

// ClosedUsage contains final cumulative totals within one generation.
type ClosedUsage struct {
	SessionID SessionID `json:"session_id"`
	RxBytes   uint64    `json:"rx_bytes"`
	TxBytes   uint64    `json:"tx_bytes"`
	PairedS   uint64    `json:"paired_s"`
}

// SessionUsage reports cumulative totals while connections are present.
type SessionUsage struct {
	ClosedUsage
	State string `json:"state"`
}

// Renewal expresses the lease relative to the heartbeat send time.
type Renewal struct {
	SessionID SessionID `json:"session_id"`
	LeaseS    int64     `json:"lease_s"`
}

// Revocation may arrive even when the session has no connections.
type Revocation struct {
	SessionID SessionID `json:"session_id"`
	Reason    string    `json:"reason"`
}

// HeartbeatResult covers every reported live session and outstanding revocations.
type HeartbeatResult struct {
	NodePolicy
	Renew  []Renewal    `json:"renew"`
	Revoke []Revocation `json:"revoke"`
}
