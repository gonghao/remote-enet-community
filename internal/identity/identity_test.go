package identity

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
)

func TestFingerprintRoundTrip(t *testing.T) {
	l, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	fp := l.Fingerprint()
	parsed, err := ParseFingerprint(fp.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed != fp {
		t.Fatalf("round trip mismatch: %s vs %s", parsed, fp)
	}
	if fp.IsZero() {
		t.Fatal("generated fingerprint is zero")
	}
	fromCert, err := FingerprintOfCert(l.Cert.Leaf)
	if err != nil {
		t.Fatal(err)
	}
	if fromCert != fp {
		t.Fatal("cert fingerprint differs from key fingerprint")
	}
}

func TestSaveLoadKeepsIdentity(t *testing.T) {
	dir := t.TempDir()
	first, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	second, err := LoadOrGenerate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Fatal("identity changed across reload")
	}
	if !first.Cert.Leaf.Equal(second.Cert.Leaf) {
		t.Fatal("certificate was re-issued although the saved one matched")
	}
}

func TestCertUsableForMutualTLS(t *testing.T) {
	l, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{l.Cert}}
	if _, err := cfg.Certificates[0].Leaf.Verify(x509.VerifyOptions{}); err == nil {
		t.Fatal("self-signed cert must not verify against system roots")
	}
	for _, u := range []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth} {
		found := false
		for _, have := range l.Cert.Leaf.ExtKeyUsage {
			found = found || have == u
		}
		if !found {
			t.Fatalf("missing ext key usage %v", u)
		}
	}
}
