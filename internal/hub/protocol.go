// Package hub authenticates outer connections and relays opaque byte streams.
package hub

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"remote-enet/internal/identity"
)

// ALPN separates routing requests from other protocols on a TLS listener.
const ALPN = "renet-hub/1"

// Role determines whether a device offers a wait slot or consumes one.
type Role byte

const (
	RoleDial Role = 1
	RoleWait Role = 2
)

var (
	ErrPeerBusy     = errors.New("hub: peer busy")
	ErrUnknownPeer  = errors.New("hub: unknown peer")
	ErrUnauthorized = errors.New("hub: unauthorized")
)

const (
	statusOK byte = iota
	statusBusy
	statusUnknownPeer
	statusUnauthorized
	maxToken     = 1024
	setupTimeout = 10 * time.Second
)

// Request contains routing metadata only. Identity always comes from TLS.
// A zero wait target allows any authorized dialer.
type Request struct {
	Role   Role
	Target identity.Fingerprint
	Token  []byte
}

func readRequest(r io.Reader) (Request, error) {
	var header [35]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return Request{}, fmt.Errorf("hub: read request: %w", err)
	}
	req := Request{Role: Role(header[0])}
	copy(req.Target[:], header[1:33])
	n := int(binary.BigEndian.Uint16(header[33:]))
	if (req.Role != RoleDial && req.Role != RoleWait) || n > maxToken || (req.Role == RoleDial && req.Target.IsZero()) {
		return Request{}, errors.New("hub: invalid request")
	}
	req.Token = make([]byte, n)
	if _, err := io.ReadFull(r, req.Token); err != nil {
		return Request{}, fmt.Errorf("hub: read token: %w", err)
	}
	return req, nil
}

func writeRequest(w io.Writer, req Request) error {
	if len(req.Token) > maxToken || (req.Role != RoleDial && req.Role != RoleWait) || (req.Role == RoleDial && req.Target.IsZero()) {
		return errors.New("hub: invalid request")
	}
	header := make([]byte, 35+len(req.Token))
	header[0] = byte(req.Role)
	copy(header[1:33], req.Target[:])
	binary.BigEndian.PutUint16(header[33:35], uint16(len(req.Token)))
	copy(header[35:], req.Token)
	return writeAll(w, header)
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) != 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func verifyPeer(cs tls.ConnectionState, expect identity.Fingerprint) error {
	if cs.NegotiatedProtocol != ALPN || len(cs.PeerCertificates) != 1 {
		return errors.New("hub: expected ALPN and one device certificate")
	}
	cert := cs.PeerCertificates[0]
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return errors.New("hub: device certificate outside validity period")
	}
	if !bytes.Equal(cert.RawSubject, cert.RawIssuer) {
		return errors.New("hub: device certificate is not self-issued")
	}
	if len(cert.UnhandledCriticalExtensions) != 0 {
		return errors.New("hub: unhandled critical certificate extension")
	}
	fp, err := identity.FingerprintOfCert(cert)
	if err != nil {
		return err
	}
	if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
		return fmt.Errorf("hub: invalid self-signature: %w", err)
	}
	if !expect.IsZero() && fp != expect {
		return errors.New("hub: fingerprint mismatch")
	}
	return nil
}

// Connect pins the hub identity and removes the routing exchange from the stream.
// After success the caller owns the connection; ctx covers establishment only.
func Connect(ctx context.Context, addr string, local *identity.Local, hubID identity.Fingerprint, req Request) (net.Conn, error) {
	if local == nil || hubID.IsZero() {
		return nil, errors.New("hub: local identity and hub fingerprint required")
	}
	ctx, cancel := context.WithTimeout(ctx, setupTimeout)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("hub: dial %s: %w", addr, err)
	}
	conn := tls.Client(raw, &tls.Config{
		MinVersion:   tls.VersionTLS13,
		MaxVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{local.Cert},
		NextProtos:   []string{ALPN},
		// The pinned Ed25519 identity replaces CA and hostname validation.
		InsecureSkipVerify: true,
		VerifyConnection:   func(cs tls.ConnectionState) error { return verifyPeer(cs, hubID) },
	})
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	success := false
	defer func() {
		stop()
		if !success {
			_ = raw.Close()
		}
	}()
	deadline, _ := ctx.Deadline()
	_ = raw.SetDeadline(deadline)
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("hub: TLS handshake: %w", err)
	}
	if err := writeRequest(conn, req); err != nil {
		return nil, fmt.Errorf("hub: send request: %w", err)
	}
	var status [1]byte
	if _, err := io.ReadFull(conn, status[:]); err != nil {
		return nil, fmt.Errorf("hub: read status: %w", err)
	}
	switch status[0] {
	case statusOK:
	case statusBusy:
		return nil, ErrPeerBusy
	case statusUnknownPeer:
		return nil, ErrUnknownPeer
	case statusUnauthorized:
		return nil, ErrUnauthorized
	default:
		return nil, fmt.Errorf("hub: invalid status %d", status[0])
	}
	if !stop() || ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("hub: clear deadline: %w", err)
	}
	success = true
	return &connection{Conn: conn, raw: raw, done: make(chan struct{})}, nil
}

func serverTLS(local *identity.Local) *tls.Config {
	return &tls.Config{
		MinVersion:       tls.VersionTLS13,
		MaxVersion:       tls.VersionTLS13,
		Certificates:     []tls.Certificate{local.Cert},
		NextProtos:       []string{ALPN},
		ClientAuth:       tls.RequireAnyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error { return verifyPeer(cs, identity.Fingerprint{}) },
	}
}
