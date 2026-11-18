package hub

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/binary"
	"io"
	"math/big"
	"net"
	"testing"
	"testing/iotest"
	"time"

	"remote-enet/internal/identity"
)

func TestRequestCodec(t *testing.T) {
	want := Request{Role: RoleDial, Target: identity.Fingerprint{1}, Token: []byte("token")}
	var b bytes.Buffer
	if err := writeRequest(&b, want); err != nil {
		t.Fatal(err)
	}
	raw := append([]byte(nil), b.Bytes()...)
	if len(raw) != 40 || raw[0] != 1 || raw[1] != 1 || binary.BigEndian.Uint16(raw[33:35]) != 5 {
		t.Fatalf("bad wire vector: %x", raw)
	}
	got, err := readRequest(iotest.OneByteReader(&b))
	if err != nil || got.Role != want.Role || got.Target != want.Target || !bytes.Equal(got.Token, want.Token) {
		t.Fatalf("roundtrip: %+v, %v", got, err)
	}
	for n := range len(raw) {
		if _, err := readRequest(bytes.NewReader(raw[:n])); err == nil {
			t.Fatalf("accepted truncation at %d", n)
		}
	}
	for _, mutate := range []func([]byte){
		func(p []byte) { p[0] = 99 },
		func(p []byte) { clear(p[1:33]) },
		func(p []byte) { binary.BigEndian.PutUint16(p[33:35], maxToken+1) },
	} {
		p := append([]byte(nil), raw...)
		mutate(p)
		if _, err := readRequest(bytes.NewReader(p)); err == nil {
			t.Fatal("accepted invalid header")
		}
	}
	if err := writeRequest(io.Discard, Request{Role: RoleWait, Token: make([]byte, maxToken+1)}); err == nil {
		t.Fatal("accepted oversized token")
	}
}

func invalidLocal(t *testing.T, kind string) *identity.Local {
	t.Helper()
	l := local(t)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "device"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	parent := tmpl
	switch kind {
	case "expired":
		tmpl.NotAfter = time.Now().Add(-time.Minute)
	case "future":
		tmpl.NotBefore = time.Now().Add(time.Minute)
	case "not-self-issued":
		copy := *tmpl
		copy.Subject = pkix.Name{CommonName: "other issuer"}
		parent = &copy
	case "critical-extension":
		tmpl.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, l.Key.Public(), l.Key)
	if err != nil {
		t.Fatal(err)
	}
	if kind == "bad-signature" {
		der[len(der)-1] ^= 1
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	l.Cert = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: l.Key, Leaf: leaf}
	return l
}

func TestInvalidDeviceCertificates(t *testing.T) {
	goodHub, goodDevice := local(t), local(t)
	_, addr, _ := start(t, Options{Local: goodHub})
	for _, kind := range []string{"expired", "future", "not-self-issued", "critical-extension", "bad-signature"} {
		t.Run(kind, func(t *testing.T) {
			bad := invalidLocal(t, kind)
			if c, err := Connect(t.Context(), addr, bad, goodHub.Fingerprint(), Request{Role: RoleWait}); err == nil {
				_ = c.Close()
				t.Fatal("accepted invalid client certificate")
			}
			_, badAddr, _ := start(t, Options{Local: bad})
			if c, err := Connect(t.Context(), badAddr, goodDevice, bad.Fingerprint(), Request{Role: RoleWait}); err == nil {
				_ = c.Close()
				t.Fatal("accepted invalid hub certificate")
			}
		})
	}
}

func TestEmbeddedLatePublishAndExpiry(t *testing.T) {
	e := NewEmbedded()
	makeConn := func() *connection {
		a, b := net.Pipe()
		t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
		return &connection{Conn: a, raw: a, done: make(chan struct{})}
	}
	old, next := makeConn(), makeConn()
	if !e.reserve(old) {
		t.Fatal("initial reservation")
	}
	_ = old.Close()
	if !e.reserve(next) {
		t.Fatal("new reservation")
	}
	if e.publish(old, identity.Fingerprint{1}) {
		t.Fatal("stale publish succeeded")
	}
	if !e.publish(next, identity.Fingerprint{2}) {
		t.Fatal("current publish failed")
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c, peer, err := e.Accept(ctx)
	if err != nil || c != next || peer != (identity.Fingerprint{2}) {
		t.Fatalf("accept: %v %v %v", c, peer, err)
	}
	_ = c.Close()
	pending := makeConn()
	if !e.reserve(pending) || !e.publish(pending, identity.Fingerprint{3}) {
		t.Fatal("pending reservation")
	}
	e.mu.Lock()
	e.timer.Reset(time.Millisecond)
	e.mu.Unlock()
	select {
	case <-pending.done:
	case <-time.After(time.Second):
		t.Fatal("unclaimed handoff did not expire")
	}
	if !e.reserve(makeConn()) {
		t.Fatal("expired handoff kept slot busy")
	}
}
