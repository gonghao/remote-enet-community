// Package trust persists pairing decisions without storing invitation tokens.
package trust

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	jsonv2 "encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

var (
	ErrCorrupt  = errors.New("trust: invalid state file")
	ErrNotFound = errors.New("trust: peer not found")

	errInvalidTicket = errors.New("trust: invalid endpoint ticket")
)

const maxTicketSize = 1024

var ticketEncoding = base64.RawURLEncoding.Strict()

type endpointTicket string

func (t endpointTicket) MarshalJSON() ([]byte, error) {
	return json.Marshal(ticketEncoding.EncodeToString([]byte(t)))
}

func (t *endpointTicket) UnmarshalJSON(raw []byte) error {
	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil ||
		encoded == "" || len(encoded) > ticketEncoding.EncodedLen(maxTicketSize) ||
		strings.ContainsAny(encoded, "\r\n") {
		return errInvalidTicket
	}
	decoded, err := ticketEncoding.DecodeString(encoded)
	if err != nil {
		return errInvalidTicket
	}
	*t = endpointTicket(decoded)
	return nil
}

type endpointRecord struct {
	Kind   string         `json:"kind"`
	Addr   string         `json:"addr"`
	Hub    string         `json:"hub"`
	Ticket endpointTicket `json:"ticket,omitempty"`
}

func (e endpointRecord) endpoint() (transport.Endpoint, error) {
	fp, err := identity.ParseFingerprint(e.Hub)
	if err != nil {
		return transport.Endpoint{}, err
	}
	return transport.Endpoint{Kind: e.Kind, Addr: e.Addr, Hub: fp, Ticket: string(e.Ticket)}, nil
}

type state struct {
	Version int                         `json:"version"`
	Trusted map[string]bool             `json:"trusted"`
	Invites map[string]int64            `json:"invites"`
	Peers   map[string][]endpointRecord `json:"peers"`
}

// Store refreshes the file under a directory lock for every operation. This
// permits an invite command to run while the operator process is accepting.
type Store struct {
	path string
}

func Open(ctx context.Context, dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("trust: state directory required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("trust: create state directory: %w", err)
	}
	s := &Store{path: filepath.Join(dir, "trust.json")}
	if err := s.access(ctx, func(*state) (bool, error) { return false, nil }); err != nil {
		return nil, err
	}
	return s, nil
}

// LoadIdentity serializes first identity creation with other commands using
// the same state directory, so invite and operator cannot mint different keys.
func (s *Store) LoadIdentity(ctx context.Context) (*identity.Local, error) {
	var local *identity.Local
	err := s.access(ctx, func(*state) (bool, error) {
		var err error
		local, err = identity.LoadOrGenerate(filepath.Join(filepath.Dir(s.path), "identity"))
		return false, err
	})
	return local, err
}

// Issue returns a secret once; only its SHA-256 and expiry reach the state file.
func (s *Store) Issue(ctx context.Context, expires time.Time) ([16]byte, error) {
	var token [16]byte
	if !expires.After(time.Now()) {
		return token, fmt.Errorf("trust: invitation expiry must be in the future")
	}
	if _, err := rand.Read(token[:]); err != nil {
		return token, fmt.Errorf("trust: generate invitation token: %w", err)
	}
	err := s.access(ctx, func(st *state) (bool, error) {
		now := time.Now().UnixNano()
		if expires.UnixNano() <= now {
			return false, fmt.Errorf("trust: invitation expired before persistence")
		}
		for hash, deadline := range st.Invites {
			if deadline <= now {
				delete(st.Invites, hash)
			}
		}
		st.Invites[tokenHash(token)] = expires.UnixNano()
		return true, nil
	})
	if err != nil {
		return [16]byte{}, err
	}
	return token, nil
}

func (s *Store) Trusted(ctx context.Context, peer identity.Fingerprint) (bool, error) {
	var trusted bool
	err := s.access(ctx, func(st *state) (bool, error) {
		trusted = st.Trusted[peer.String()]
		return false, nil
	})
	return trusted, err
}

// Consume commits token removal and trust together. Invalid, expired, and
// already-consumed tokens return false without changing either collection.
func (s *Store) Consume(ctx context.Context, peer identity.Fingerprint, token [16]byte) (bool, error) {
	if peer.IsZero() || token == [16]byte{} {
		return false, nil
	}
	accepted := false
	err := s.access(ctx, func(st *state) (bool, error) {
		hash := tokenHash(token)
		expires, ok := st.Invites[hash]
		if !ok || time.Now().UnixNano() >= expires {
			return false, nil
		}
		delete(st.Invites, hash)
		st.Trusted[peer.String()] = true
		accepted = true
		return true, nil
	})
	return accepted && err == nil, err
}

func (s *Store) SavePeer(ctx context.Context, peer transport.PeerInfo) error {
	if peer.ID.IsZero() || len(peer.Endpoints) == 0 {
		return fmt.Errorf("trust: peer identity and endpoints required")
	}
	endpoints := make([]endpointRecord, 0, len(peer.Endpoints))
	for _, e := range peer.Endpoints {
		if err := validEndpoint(e.Kind, e.Addr, e.Hub.String()); err != nil {
			return err
		}
		if len(e.Ticket) > maxTicketSize {
			return errInvalidTicket
		}
		endpoints = append(endpoints, endpointRecord{Kind: e.Kind, Addr: e.Addr, Hub: e.Hub.String(), Ticket: endpointTicket(e.Ticket)})
	}
	return s.access(ctx, func(st *state) (bool, error) {
		st.Peers[peer.ID.String()] = endpoints
		return true, nil
	})
}

// UpdateEndpointTicket replaces a ticket atomically. The peer, Hub fingerprint
// and address must identify exactly one saved endpoint, and ticket is raw bytes.
func (s *Store) UpdateEndpointTicket(ctx context.Context, peer, hub identity.Fingerprint, addr, ticket string) error {
	if len(ticket) == 0 || len(ticket) > maxTicketSize {
		return errInvalidTicket
	}
	return s.access(ctx, func(st *state) (bool, error) {
		entries := st.Peers[peer.String()]
		var match *endpointRecord
		for i := range entries {
			e := &entries[i]
			if e.Addr != addr {
				continue
			}
			fp, err := identity.ParseFingerprint(e.Hub)
			if err != nil {
				return false, err
			}
			if fp != hub {
				continue
			}
			if match != nil {
				return false, fmt.Errorf("trust: multiple saved endpoints match")
			}
			match = e
		}
		if match == nil {
			return false, fmt.Errorf("%w: saved endpoint", ErrNotFound)
		}
		if match.Ticket == endpointTicket(ticket) {
			return false, nil
		}
		match.Ticket = endpointTicket(ticket)
		return true, nil
	})
}

func (s *Store) Peer(ctx context.Context, id identity.Fingerprint) (transport.PeerInfo, error) {
	peer := transport.PeerInfo{ID: id}
	err := s.access(ctx, func(st *state) (bool, error) {
		entries, ok := st.Peers[id.String()]
		if !ok {
			return false, fmt.Errorf("%w: %s", ErrNotFound, id)
		}
		for _, e := range entries {
			endpoint, err := e.endpoint()
			if err != nil {
				return false, err
			}
			peer.Endpoints = append(peer.Endpoints, endpoint)
		}
		return false, nil
	})
	return peer, err
}

func tokenHash(token [16]byte) string {
	sum := sha256.Sum256(token[:])
	return hex.EncodeToString(sum[:])
}

func (s *Store) access(ctx context.Context, op func(*state) (bool, error)) (result error) {
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := unlock(); err != nil {
			result = errors.Join(result, fmt.Errorf("trust: release state lock: %w", err))
		}
	}()
	st, err := s.load()
	if err != nil {
		return err
	}
	changed, err := op(st)
	if err != nil {
		return err
	}
	if !changed {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.save(st)
}

func (s *Store) lock(ctx context.Context) (func() error, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	lockPath := s.path + ".lock"
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("trust: acquire state lock: %w", errors.Join(last, err))
		}
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			return func() error { return os.Remove(lockPath) }, nil
		} else if !retryLockCreation(err) {
			return nil, fmt.Errorf("trust: create state lock: %w", err)
		} else {
			last = err
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("trust: acquire state lock %s: %w", lockPath, errors.Join(last, ctx.Err()))
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func emptyState() *state {
	return &state{Version: 1, Trusted: map[string]bool{}, Invites: map[string]int64{}, Peers: map[string][]endpointRecord{}}
}

func (s *Store) load() (*state, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return emptyState(), nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: open state: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("trust: stat state: %w", err)
	}
	if info.Size() > 1<<20 {
		return nil, fmt.Errorf("%w: state exceeds 1 MiB", ErrCorrupt)
	}
	return decodeState(f)
}

func decodeState(f io.Reader) (*state, error) {
	raw, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read state: %w", ErrCorrupt, err)
	}
	if len(raw) > 1<<20 {
		return nil, fmt.Errorf("%w: state exceeds 1 MiB", ErrCorrupt)
	}
	var header struct {
		Version int `json:"version"`
	}
	// Select the schema before validating fields that a future version could add.
	if err := jsonv2.Unmarshal(raw, &header); err != nil {
		return nil, fmt.Errorf("%w: cannot decode state", ErrCorrupt)
	}
	if header.Version != 1 && header.Version != 2 {
		return nil, fmt.Errorf("%w: unsupported version", ErrCorrupt)
	}
	var st state
	if err := jsonv2.Unmarshal(raw, &st, jsonv2.RejectUnknownMembers(true)); err != nil {
		if errors.Is(err, errInvalidTicket) {
			return nil, fmt.Errorf("%w: %w", ErrCorrupt, errInvalidTicket)
		}
		// JSON decoder errors may quote input from a credential-bearing file.
		return nil, fmt.Errorf("%w: cannot decode state", ErrCorrupt)
	}
	if st.Trusted == nil || st.Invites == nil || st.Peers == nil {
		return nil, fmt.Errorf("%w: missing collections", ErrCorrupt)
	}
	for key, trusted := range st.Trusted {
		fp, err := identity.ParseFingerprint(key)
		if err != nil || fp.IsZero() || !trusted {
			return nil, fmt.Errorf("%w: invalid trusted identity", ErrCorrupt)
		}
	}
	for hash, expires := range st.Invites {
		raw, err := hex.DecodeString(hash)
		if err != nil || len(raw) != sha256.Size || expires <= 0 {
			return nil, fmt.Errorf("%w: invalid invitation record", ErrCorrupt)
		}
	}
	for key, endpoints := range st.Peers {
		fp, err := identity.ParseFingerprint(key)
		if err != nil || fp.IsZero() || len(endpoints) == 0 {
			return nil, fmt.Errorf("%w: invalid saved peer", ErrCorrupt)
		}
		for _, e := range endpoints {
			if err := validEndpoint(e.Kind, e.Addr, e.Hub); err != nil {
				return nil, fmt.Errorf("%w: %w", ErrCorrupt, err)
			}
			if st.Version == 1 && e.Ticket != "" {
				return nil, fmt.Errorf("%w: endpoint ticket requires version 2", ErrCorrupt)
			}
		}
	}
	return &st, nil
}

func validEndpoint(kind, addr, hub string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("trust: endpoint must be host:port")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("trust: endpoint port must be in 1..65535")
	}
	fp, err := identity.ParseFingerprint(hub)
	if kind != "hub" || err != nil || fp.IsZero() {
		return fmt.Errorf("trust: endpoint kind and hub identity required")
	}
	return nil
}

func (s *Store) save(st *state) error {
	st.Version = 1
	for _, endpoints := range st.Peers {
		for _, e := range endpoints {
			if e.Ticket != "" {
				st.Version = 2
				break
			}
		}
		if st.Version == 2 {
			break
		}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("trust: encode state: %w", err)
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("trust: state exceeds 1 MiB")
	}
	f, err := os.CreateTemp(filepath.Dir(s.path), ".trust-*.tmp")
	if err != nil {
		return fmt.Errorf("trust: create temporary state: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(raw); err != nil {
		f.Close()
		return fmt.Errorf("trust: write state: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("trust: sync state: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("trust: close state: %w", err)
	}
	if err := os.Rename(f.Name(), s.path); err != nil {
		return fmt.Errorf("trust: replace state: %w", err)
	}
	return nil
}

// Remember commits direct pairing without consuming an invitation.
func (s *Store) Remember(ctx context.Context, peer identity.Fingerprint) error {
	if peer.IsZero() {
		return fmt.Errorf("trust: peer identity required")
	}
	return s.access(ctx, func(st *state) (bool, error) {
		if st.Trusted[peer.String()] {
			return false, nil
		}
		st.Trusted[peer.String()] = true
		return true, nil
	})
}
