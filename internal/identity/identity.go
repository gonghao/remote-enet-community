// Package identity holds the device key pair and the self-signed certificate
// derived from it. A device is identified everywhere by the SHA-256 of its raw
// Ed25519 public key, so re-issuing the certificate never changes the identity.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ErrFingerprintMismatch identifies a peer that failed its configured pin.
var ErrFingerprintMismatch = errors.New("fingerprint mismatch")

// Fingerprint is SHA-256 over the 32 raw bytes of an Ed25519 public key.
type Fingerprint [sha256.Size]byte

// IsZero reports whether no fingerprint is set.
func (f Fingerprint) IsZero() bool { return f == Fingerprint{} }

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// String renders the full fingerprint as upper-case base32 in groups of four,
// which is what invite codes and config files carry.
func (f Fingerprint) String() string {
	s := b32.EncodeToString(f[:])
	var sb strings.Builder
	for i := 0; i < len(s); i += 4 {
		if i > 0 {
			sb.WriteByte('-')
		}
		end := min(i+4, len(s))
		sb.WriteString(s[i:end])
	}
	return sb.String()
}

// Short is the first two groups, for UI labels only; never compare on it.
func (f Fingerprint) Short() string {
	return f.String()[:9]
}

// ParseFingerprint accepts the String() form with or without dashes.
func ParseFingerprint(s string) (Fingerprint, error) {
	raw, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(s, "-", "")))
	if err != nil {
		return Fingerprint{}, fmt.Errorf("identity: bad fingerprint %q: %w", s, err)
	}
	if len(raw) != sha256.Size {
		return Fingerprint{}, fmt.Errorf("identity: bad fingerprint %q: %d bytes", s, len(raw))
	}
	var f Fingerprint
	copy(f[:], raw)
	return f, nil
}

// FingerprintOf computes the fingerprint of a public key.
func FingerprintOf(pub ed25519.PublicKey) Fingerprint {
	return sha256.Sum256(pub)
}

// FingerprintOfCert extracts the Ed25519 key from a peer certificate. Any
// other key type is rejected: it cannot belong to a remote-enet device.
func FingerprintOfCert(cert *x509.Certificate) (Fingerprint, error) {
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return Fingerprint{}, fmt.Errorf("identity: certificate key is %T, want ed25519", cert.PublicKey)
	}
	return FingerprintOf(pub), nil
}

// Local is this device's identity: the private key and a self-signed
// certificate usable for both TLS client and server authentication.
type Local struct {
	Key  ed25519.PrivateKey
	Cert tls.Certificate
	fp   Fingerprint
}

// Fingerprint of the local device.
func (l *Local) Fingerprint() Fingerprint { return l.fp }

const (
	keyFile  = "device.key"
	certFile = "device.crt"
	// Certificates are long-lived on purpose: identity is the key, not the
	// cert, and nothing validates expiry against a CA.
	certValidity = 20 * 365 * 24 * time.Hour
)

// Generate creates a fresh identity in memory.
func Generate() (*Local, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("identity: generate key: %w", err)
	}
	return fromKey(priv, pub)
}

func fromKey(priv ed25519.PrivateKey, pub ed25519.PublicKey) (*Local, error) {
	fp := FingerprintOf(pub)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("identity: serial: %w", err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: fp.String()},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(certValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, fmt.Errorf("identity: self-sign: %w", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("identity: parse own cert: %w", err)
	}
	return &Local{
		Key:  priv,
		Cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv, Leaf: leaf},
		fp:   fp,
	}, nil
}

// Save writes the key and certificate as PEM into dir (created if missing).
// The key file is mode 0600.
func (l *Local) Save(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("identity: mkdir: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(l.Key)
	if err != nil {
		return fmt.Errorf("identity: marshal key: %w", err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
		return fmt.Errorf("identity: write key: %w", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: l.Cert.Certificate[0]})
	if err := os.WriteFile(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
		return fmt.Errorf("identity: write cert: %w", err)
	}
	return nil
}

// Load reads an identity saved by Save. The certificate is re-derived from
// the key if the cert file is missing or does not match it.
func Load(dir string) (*Local, error) {
	keyPEM, err := os.ReadFile(filepath.Join(dir, keyFile))
	if err != nil {
		return nil, fmt.Errorf("identity: read key: %w", err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("identity: key file is not a PRIVATE KEY PEM block")
	}
	anyKey, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("identity: parse key: %w", err)
	}
	priv, ok := anyKey.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("identity: key is %T, want ed25519", anyKey)
	}
	pub := priv.Public().(ed25519.PublicKey)

	certPEM, err := os.ReadFile(filepath.Join(dir, certFile))
	if err == nil {
		if cb, _ := pem.Decode(certPEM); cb != nil && cb.Type == "CERTIFICATE" {
			if leaf, perr := x509.ParseCertificate(cb.Bytes); perr == nil {
				if lp, ok := leaf.PublicKey.(ed25519.PublicKey); ok && lp.Equal(pub) {
					return &Local{
						Key:  priv,
						Cert: tls.Certificate{Certificate: [][]byte{cb.Bytes}, PrivateKey: priv, Leaf: leaf},
						fp:   FingerprintOf(pub),
					}, nil
				}
			}
		}
	}
	l, err := fromKey(priv, pub)
	if err != nil {
		return nil, err
	}
	if err := l.Save(dir); err != nil {
		return nil, err
	}
	return l, nil
}

// LoadOrGenerate is what binaries call at startup.
func LoadOrGenerate(dir string) (*Local, error) {
	l, err := Load(dir)
	if err == nil {
		return l, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	l, err = Generate()
	if err != nil {
		return nil, err
	}
	if err := l.Save(dir); err != nil {
		return nil, err
	}
	return l, nil
}
