package ticket

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"remote-enet/internal/identity"
)

// WaitTTL bounds one registration. DialTTL is renewed at each handshake.
const (
	WaitTTL = 10 * time.Minute
	DialTTL = 30 * 24 * time.Hour
)

// Issuer owns the signing key, separate from the device identity.
type Issuer struct{ priv ed25519.PrivateKey }

// LoadOrCreateIssuer publishes a complete PKCS#8 file without replacing a concurrent winner.
func LoadOrCreateIssuer(path string) (*Issuer, error) {
	i, err := readIssuer(path)
	if !errors.Is(err, os.ErrNotExist) {
		return i, err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create issuer directory: %w", err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate issuer key: %w", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, fmt.Errorf("marshal issuer key: %w", err)
	}
	// CreateTemp uses O_EXCL and mode 0600. Link publishes only after close,
	// with exclusive-create semantics for the final name as well.
	f, err := os.CreateTemp(dir, ".issuer-*")
	if err != nil {
		return nil, fmt.Errorf("create temporary issuer key: %w", err)
	}
	defer os.Remove(f.Name())
	err = pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	closeErr := f.Close()
	if err != nil {
		return nil, fmt.Errorf("write issuer key: %w", err)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close issuer key: %w", closeErr)
	}
	if err = os.Link(f.Name(), path); errors.Is(err, os.ErrExist) {
		return readIssuer(path)
	} else if err != nil {
		return nil, fmt.Errorf("publish issuer key: %w", err)
	}
	return &Issuer{priv}, nil
}

func readIssuer(path string) (*Issuer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read issuer key: %w", err)
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" || !bytes.HasPrefix(data, []byte("-----BEGIN PRIVATE KEY-----")) || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("invalid issuer PEM")
	}
	key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse issuer PKCS#8: %w", err)
	}
	priv, ok := key.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("issuer key must be Ed25519")
	}
	return &Issuer{priv}, nil
}

// PublicKey returns an independent copy for the Hub trust configuration.
func (i *Issuer) PublicKey() ed25519.PublicKey { return i.priv.Public().(ed25519.PublicKey) }

// Kid identifies this issuer's tickets.
func (i *Issuer) Kid() Kid { return KidOf(i.PublicKey()) }

func (i *Issuer) issue(now, expires time.Time, c Claims) ([]byte, [16]byte, error) {
	c.IssuedAt = now
	c.NotBefore = now
	c.Expires = expires
	rand.Read(c.JTI[:])
	raw, err := Sign(i.priv, c)
	if err != nil {
		return nil, [16]byte{}, err
	}
	return raw, c.JTI, nil
}

// Wait uses a zero pairUntil to close the pairing entrance.
func (i *Issuer) Wait(now time.Time, dev, hub identity.Fingerprint, pairUntil time.Time) ([]byte, error) {
	raw, _, err := i.issue(now, now.Add(WaitTTL), Claims{Role: roleWait, Device: dev, Hub: hub, PairUntil: pairUntil})
	return raw, err
}

// Dial binds the ticket to the client's TLS identity.
func (i *Issuer) Dial(now time.Time, client, operator, hub identity.Fingerprint) ([]byte, error) {
	raw, _, err := i.issue(now, now.Add(DialTTL), Claims{Role: roleDial, Device: client, Peer: operator, Hub: hub})
	return raw, err
}

// Bootstrap returns the bearer ticket and its revocation handle.
func (i *Issuer) Bootstrap(now, expires time.Time, operator, hub identity.Fingerprint) ([]byte, [16]byte, error) {
	return i.issue(now, expires, Claims{Role: roleDial, Bearer: true, Peer: operator, Hub: hub})
}
