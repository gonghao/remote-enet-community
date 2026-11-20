package centerstate

import (
	"context"
	"encoding/base64"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
)

const (
	version        = 1
	maxRecordBytes = 64 << 10
)

var idEncoding = base64.RawURLEncoding.Strict()

type recordID [16]byte

func (id recordID) MarshalText() ([]byte, error) {
	return idEncoding.AppendEncode(nil, id[:]), nil
}

func (id *recordID) UnmarshalText(text []byte) error {
	if len(text) != idEncoding.EncodedLen(len(id)) {
		return fmt.Errorf("expected a 22-character base64url ID")
	}
	if n, err := idEncoding.Decode(id[:], text); err != nil || n != len(id) {
		return fmt.Errorf("invalid base64url ID")
	}
	return nil
}

func pinFilename(id [16]byte) string {
	return idEncoding.EncodeToString(id[:]) + ".json"
}

type fingerprint identity.Fingerprint

func (f fingerprint) MarshalText() ([]byte, error) {
	return []byte(identity.Fingerprint(f).String()), nil
}

func (f *fingerprint) UnmarshalText(text []byte) error {
	value := string(text)
	fp, err := identity.ParseFingerprint(value)
	if err != nil {
		return err
	}
	canonical := fp.String()
	if value != canonical && value != strings.ReplaceAll(canonical, "-", "") {
		return fmt.Errorf("noncanonical fingerprint")
	}
	*f = fingerprint(fp)
	return nil
}

type nodeRecord struct {
	ID    recordID    `json:"node_id"`
	Addr  string      `json:"addr"`
	Port  uint16      `json:"port"`
	HubFP fingerprint `json:"hub_fp"`
}

type pinRecord struct {
	Version  int         `json:"version"`
	CenterID string      `json:"center_id"`
	Session  recordID    `json:"session_id"`
	Role     string      `json:"role"`
	Peer     fingerprint `json:"peer_fp"`
	Node     nodeRecord  `json:"node"`
	PairedAt time.Time   `json:"paired_at"`
}

func encodePin(p Pin) pinRecord {
	return pinRecord{
		Version:  version,
		CenterID: p.CenterID,
		Session:  recordID(p.Session),
		Role:     p.Role,
		Peer:     fingerprint(p.Peer),
		Node: nodeRecord{
			ID:    recordID(p.Node.ID),
			Addr:  p.Node.Addr,
			Port:  p.Node.Port,
			HubFP: fingerprint(p.Node.HubFP),
		},
		PairedAt: p.PairedAt.UTC(),
	}
}

func (r pinRecord) pin() Pin {
	return Pin{
		CenterID: r.CenterID,
		Session:  [16]byte(r.Session),
		Role:     r.Role,
		Peer:     identity.Fingerprint(r.Peer),
		Node: NodeRef{
			ID:    [16]byte(r.Node.ID),
			Addr:  r.Node.Addr,
			Port:  r.Node.Port,
			HubFP: identity.Fingerprint(r.Node.HubFP),
		},
		PairedAt: r.PairedAt,
	}
}

func (s *Store) validatePin(id [16]byte, p Pin) error {
	switch {
	case p.CenterID != s.centerID:
		return fmt.Errorf("pin center_id %q does not match store center %q", p.CenterID, s.centerID)
	case p.Session == [16]byte{} || p.Session != id:
		return fmt.Errorf("pin session_id must be nonzero and match the filename")
	case p.Role != settings.RoleOperator && p.Role != settings.RoleClient:
		return fmt.Errorf("invalid pin role %q", p.Role)
	case p.Peer.IsZero():
		return fmt.Errorf("pin peer_fp is required")
	case p.Node.ID == [16]byte{} || strings.TrimSpace(p.Node.Addr) == "" || p.Node.Port == 0 || p.Node.HubFP.IsZero():
		return fmt.Errorf("pin node_id, addr, port and hub_fp are required")
	case p.PairedAt.IsZero():
		return fmt.Errorf("pin paired_at is required")
	}
	return nil
}

type loginRecord struct {
	Version      int         `json:"version"`
	DeviceFP     fingerprint `json:"device_fp"`
	DeviceLabel  *string     `json:"device_label"`
	AccountID    string      `json:"account_id"`
	AccountLabel *string     `json:"account_label"`
	ExpiresAt    time.Time   `json:"expires_at"`
	LoggedInAt   time.Time   `json:"logged_in_at"`
}

func encodeLogin(l Login) loginRecord {
	return loginRecord{
		Version:      version,
		DeviceFP:     fingerprint(l.DeviceFP),
		DeviceLabel:  &l.DeviceLabel,
		AccountID:    l.AccountID,
		AccountLabel: &l.AccountLabel,
		ExpiresAt:    l.ExpiresAt.UTC(),
		LoggedInAt:   l.LoggedInAt.UTC(),
	}
}

func (r loginRecord) login() (Login, error) {
	if r.Version != version {
		return Login{}, fmt.Errorf("unsupported version %d", r.Version)
	}
	if r.DeviceLabel == nil || r.AccountLabel == nil {
		return Login{}, fmt.Errorf("device_label and account_label are required")
	}
	l := Login{
		DeviceFP:     identity.Fingerprint(r.DeviceFP),
		DeviceLabel:  *r.DeviceLabel,
		AccountID:    r.AccountID,
		AccountLabel: *r.AccountLabel,
		ExpiresAt:    r.ExpiresAt,
		LoggedInAt:   r.LoggedInAt,
	}
	return l, validateLogin(l)
}

func validateLogin(l Login) error {
	var accountID recordID
	if err := accountID.UnmarshalText([]byte(l.AccountID)); err != nil {
		return fmt.Errorf("invalid account_id: %w", err)
	}
	if l.DeviceFP.IsZero() || l.ExpiresAt.IsZero() || l.LoggedInAt.IsZero() {
		return fmt.Errorf("device_fp, expires_at and logged_in_at are required")
	}
	return nil
}

func readRecord(dir *os.Root, name string, record any) (bool, error) {
	info, err := dir.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect record: %w", err)
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("%w: record must be a regular file", ErrCorrupt)
	}
	f, err := dir.OpenFile(name, os.O_RDONLY|snapshotFlags, 0)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open record: %w", err)
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil {
		return false, fmt.Errorf("inspect open record: %w", err)
	}
	if !opened.Mode().IsRegular() || opened.Size() > maxRecordBytes {
		return false, fmt.Errorf("%w: record must be a regular file of at most %d bytes", ErrCorrupt, maxRecordBytes)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxRecordBytes+1))
	if err != nil {
		return false, fmt.Errorf("read record: %w", err)
	}
	if len(data) > maxRecordBytes {
		return false, fmt.Errorf("%w: record exceeds %d bytes", ErrCorrupt, maxRecordBytes)
	}
	if err := json.Unmarshal(data, record, json.RejectUnknownMembers(true)); err != nil {
		return false, fmt.Errorf("%w: decode record: %w", ErrCorrupt, err)
	}
	return true, nil
}

func saveRecord(ctx context.Context, dir *os.Root, name string, record any) error {
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode record: %w", err)
	}
	data = append(data, '\n')
	if len(data) > maxRecordBytes {
		return fmt.Errorf("record exceeds %d bytes", maxRecordBytes)
	}
	return replaceRecord(ctx, dir, name, data)
}
