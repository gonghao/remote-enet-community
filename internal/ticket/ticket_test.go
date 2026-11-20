package ticket

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/fxamacker/cbor/v2"
	"remote-enet/internal/identity"
)

func private() ed25519.PrivateKey {
	var seed [32]byte
	for i := range seed {
		seed[i] = byte(i)
	}
	return ed25519.NewKeyFromSeed(seed[:])
}
func lookup(k Kid) (ed25519.PublicKey, bool) {
	pub := private().Public().(ed25519.PublicKey)
	return pub, k == KidOf(pub)
}
func fixture(name string) Claims {
	now := time.Unix(1700000000, 0).UTC()
	c := Claims{Role: roleDial, Device: identity.Fingerprint(bytes.Repeat([]byte{1}, 32)), Peer: identity.Fingerprint(bytes.Repeat([]byte{2}, 32)), Hub: identity.Fingerprint(bytes.Repeat([]byte{3}, 32)), IssuedAt: now, NotBefore: now, Expires: now.Add(DialTTL)}
	for i := range c.JTI {
		c.JTI[i] = byte(i)
	}
	switch name {
	case "wait":
		c.Role = roleWait
		c.Device = c.Peer
		c.Peer = identity.Fingerprint{}
		c.Expires = now.Add(WaitTTL)
		c.PairUntil = now.Add(5 * time.Minute)
	case "bootstrap":
		c.Bearer = true
		c.Device = identity.Fingerprint{}
		c.Expires = now.Add(time.Hour)
	}
	return c
}
func partsOf(t *testing.T, raw []byte) []any {
	t.Helper()
	var tag cbor.RawTag
	var parts []any
	if err := strict.Unmarshal(raw, &tag); err != nil {
		t.Fatal(err)
	}
	if err := strict.Unmarshal(tag.Content, &parts); err != nil {
		t.Fatal(err)
	}
	return parts
}
func TestVectors(t *testing.T) {
	for _, name := range []string{"wait", "dial", "bootstrap"} {
		t.Run(name, func(t *testing.T) {
			c := fixture(name)
			raw := must(Sign(private(), c))
			parts := partsOf(t, raw)
			prot := parts[0].([]byte)
			payload := parts[2].([]byte)
			kid, got, err := Verify(raw, lookup)
			if err != nil || got != c {
				t.Fatalf("verify: %+v %v", got, err)
			}
			ik, ic, err := Inspect(raw)
			if ik != kid || ic != c || err != nil {
				t.Fatal("inspect", err)
			}
			var vector map[string]string
			if err := json.Unmarshal(must(os.ReadFile("testdata/"+name+".json")), &vector); err != nil {
				t.Fatal(err)
			}
			for field, b := range map[string][]byte{"kid": kid[:], "protected": prot, "payload": payload, "input": signatureInput(prot, payload), "raw": raw} {
				if hex.EncodeToString(b) != vector[field] {
					t.Errorf("%s mismatch", field)
				}
			}
			c.Expires = time.Unix(math.MaxUint32, 0)
			if len(must(Sign(private(), c))) > 256 {
				t.Fatal("exceeded size budget")
			}
		})
	}
}
func envelope(parts []any) []byte { return must(enc.Marshal(cbor.Tag{Number: 18, Content: parts})) }
func signed(parts []any, payload []byte) []byte {
	parts = append([]any(nil), parts...)
	parts[2] = payload
	parts[3] = ed25519.Sign(private(), signatureInput(parts[0].([]byte), payload))
	return envelope(parts)
}
func TestEnvelopeRejections(t *testing.T) {
	raw := must(Sign(private(), fixture("dial")))
	parts := partsOf(t, raw)
	p := parts[0].([]byte)
	kid := KidOf(private().Public().(ed25519.PublicKey))
	tests := map[string]struct {
		raw   []byte
		stage string
	}{}
	add := func(name string, b []byte, stage string) {
		tests[name] = struct {
			raw   []byte
			stage string
		}{b, stage}
	}
	mod := func(name string, i int, value any, stage string) {
		copyParts := append([]any(nil), parts...)
		copyParts[i] = value
		add(name, envelope(copyParts), stage)
	}
	mod("bad signature", 3, make([]byte, 64), "A4")
	mod("bad payload before authentication", 2, []byte{0xff}, "A4")
	mod("short signature", 3, []byte{}, "A2")
	mod("missing payload", 2, nil, "A2")
	mod("unprotected", 1, map[int]int{1: 1}, "A2")
	mod("extra protected", 0, must(enc.Marshal(map[int]any{1: -8, 4: kid[:], 7: 0})), "A2")
	mod("unknown kid", 0, must(enc.Marshal(map[int]any{1: -8, 4: make([]byte, 8)})), "A3")
	mod("bad algorithm", 0, must(enc.Marshal(map[int]any{1: -7, 4: kid[:]})), "A2")
	mod("duplicate protected", 0, append(append([]byte{0xa3}, p[1:]...), 1, 0x27), "A2")
	add("missing tag", raw[1:], "A2")
	add("trailing", append(bytes.Clone(raw), 0), "A2")
	add("oversize", make([]byte, 1025), "A2")
	add("indefinite", append(append([]byte{0xd2, 0x9f}, raw[2:]...), 0xff), "A2")
	add("invalid payload", signed(parts, []byte{0xff}), "A5")
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			_, _, err := Verify(tc.raw, lookup)
			if err == nil || !strings.Contains(err.Error(), tc.stage) {
				t.Fatalf("expected %s, got %v", tc.stage, err)
			}
		})
	}
	if _, _, err := Inspect(tests["bad signature"].raw); err != nil {
		t.Fatal(err)
	}
}
func TestPayloadProfile(t *testing.T) {
	parts := partsOf(t, must(Sign(private(), fixture("dial"))))
	payload := parts[2].([]byte)
	check := func(name string, p []byte, valid bool) {
		t.Run(name, func(t *testing.T) {
			_, _, err := Verify(signed(parts, p), lookup)
			if (err == nil) != valid {
				t.Fatalf("valid=%v, got %v", valid, err)
			}
		})
	}
	mutate := func(name string, k uint64, v any, valid bool) {
		var m map[uint64]any
		if err := strict.Unmarshal(payload, &m); err != nil {
			t.Fatal(err)
		}
		if v == nil {
			delete(m, k)
		} else {
			m[k] = v
		}
		check(name, must(enc.Marshal(m)), valid)
	}
	for _, k := range []uint64{0, 2, 3, 7, 8, 14, 17, 63} {
		mutate(fmt.Sprint("constraint", k), k, 0, false)
	}
	for _, k := range []uint64{1, 4, 5, 6, 9, 10, 11, 12, 13} {
		mutate(fmt.Sprint("missing", k), k, nil, false)
	}
	mutate("information", 64, map[int]string{1: "future"}, true)
	mutate("false bearer", 16, false, false)
	mutate("bearer with dev", 16, true, false)
	mutate("wait with peer", 5, 2, false)
	mutate("dial pair_until", 15, 1, false)
	mutate("version one", 1, 1, false)
	mutate("version three", 1, 3, false)
	mutate("wrong role", 5, 3, false)
	mutate("negative time", 10, -1, false)
	mutate("large time", 12, int64(math.MaxUint32)+1, false)
	mutate("wrong type", 9, "text", false)
	mutate("wrong length", 13, make([]byte, 17), false)
	mutate("time left to policy", 12, 1, true)
	check("duplicate payload", append(append([]byte{payload[0] + 1}, payload[1:]...), 1, 2), false)
	check("trailing payload", append(bytes.Clone(payload), 0), false)
	check("indefinite payload", append(append([]byte{0xbf}, payload[1:]...), 0xff), false)
	extra := append(append([]byte{payload[0] + 1}, payload[1:]...), 0x18, 64)
	check("deep information", append(append(bytes.Clone(extra), bytes.Repeat([]byte{0x81}, 20)...), 0), false)
	check("duplicate information map", append(bytes.Clone(extra), 0xa2, 1, 0, 1, 0), false)
}
func TestSignInvalid(t *testing.T) {
	wait := fixture("wait")
	wait.PairUntil = wait.Expires.Add(time.Second)
	if _, err := Sign(private(), wait); err == nil {
		t.Fatal("signed wait with pairing window past expiry")
	}
	for _, change := range []func(*Claims){func(c *Claims) { c.Device = identity.Fingerprint{} }, func(c *Claims) { c.Bearer = true }, func(c *Claims) { c.Role = 2 }, func(c *Claims) { c.NotBefore = c.Expires }, func(c *Claims) { c.Expires = c.IssuedAt }, func(c *Claims) { c.Expires = time.Unix(1<<32, 0) }} {
		c := fixture("dial")
		change(&c)
		if _, err := Sign(private(), c); err == nil {
			t.Fatal("signed invalid claims", c)
		}
	}
}
func TestTime(t *testing.T) {
	unix := func(s int64) time.Time { return time.Unix(s, 0) }
	const end = math.MaxUint32
	c := Claims{IssuedAt: unix(end - 10), NotBefore: unix(end - 8), Expires: unix(end)}
	for _, tc := range []struct {
		now, skew, ttl, boot, pair int64
		bearer                     bool
		until                      int64
		ok                         bool
	}{
		{end + 1, 1, 10, 0, 0, false, 0, true}, {end + 2, 1, 10, 0, 0, false, 0, false}, {end - 9, 1, 10, 0, 0, false, 0, true}, {end - 10, 1, 10, 0, 0, false, 0, false}, {end - 12, 1, 10, 0, 0, false, 0, false},
		{end, 0, 9, 10, 0, false, 0, false}, {end, 0, 10, 9, 0, true, 0, false}, {end, 0, 10, 10, 0, true, 0, true},
		{end, 0, 10, 0, 3, false, end - 7, true}, {end, 0, 10, 0, 2, false, end - 7, false}, {end, 0, 10, 0, 10, false, end - 9, false}, {end, 0, 10, 0, 12, false, end + 1, false},
		{end, 0, 10, 0, 10, false, end - 8, true},
		{end, 0, 10, 0, 10, false, end, true},
		{math.MaxInt64, math.MaxInt64, 10, 0, 0, false, 0, true}, {math.MinInt64, math.MaxInt64, 10, 0, 0, false, 0, false},
	} {
		c.Bearer = tc.bearer
		c.PairUntil = time.Time{}
		if tc.until != 0 {
			c.PairUntil = unix(tc.until)
		}
		err := CheckTime(c, unix(tc.now), tc.skew, tc.ttl, tc.boot, tc.pair)
		if (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}
func TestIssuer(t *testing.T) {
	t.Run("link", func(t *testing.T) { testIssuer(t, os.Link) })
	t.Run("exclusive create", func(t *testing.T) {
		testIssuer(t, func(string, string) error { return errors.ErrUnsupported })
	})
}
func TestIssuerPartialPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), "issuer.key")
	want := must(LoadOrCreateIssuer(path))
	data := must(os.ReadFile(path))
	if err := os.WriteFile(path, data[:8], 0600); err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		go func() {
			time.Sleep(time.Millisecond)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Error(err)
			}
		}()
		got, err := LoadOrCreateIssuer(path)
		if err != nil || got.Kid() != want.Kid() {
			t.Fatalf("read partially published key: %v", err)
		}
	})
}
func TestIssuerPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits")
	}
	path := filepath.Join(t.TempDir(), "issuer.key")
	_ = must(LoadOrCreateIssuer(path))
	for _, mode := range []os.FileMode{0640, 0777} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadOrCreateIssuer(path); err == nil || !strings.Contains(err.Error(), "chmod 600") || !strings.Contains(err.Error(), "POSIX permissions") || !strings.Contains(err.Error(), "move the state directory") {
			t.Fatalf("permissions %o: %v", mode, err)
		}
	}
}
func testIssuer(t *testing.T, link func(string, string) error) {
	path := filepath.Join(t.TempDir(), "operator", "issuer.key")
	const count = 24
	var wg sync.WaitGroup
	var issuers [count]*Issuer
	start := make(chan struct{})
	for n := range count {
		wg.Go(func() {
			<-start
			i, err := loadOrCreateIssuer(path, link)
			if err != nil {
				t.Error(err)
				return
			}
			issuers[n] = i
		})
	}
	close(start)
	wg.Wait()
	if t.Failed() {
		return
	}
	issuer := issuers[0]
	for _, i := range issuers {
		if !bytes.Equal(i.PublicKey(), issuer.PublicKey()) {
			t.Fatal("different concurrent keys")
		}
	}
	// Windows file ACLs do not round-trip Unix permission bits.
	if runtime.GOOS != "windows" && must(os.Stat(path)).Mode().Perm() != 0600 {
		t.Fatal("issuer permissions")
	}
	c := fixture("dial")
	now := c.IssuedAt
	wait := must(issuer.Wait(now, c.Peer, c.Hub, now.Add(time.Minute)))
	dial := must(issuer.Dial(now, c.Device, c.Peer, c.Hub))
	boot, jti, err := issuer.Bootstrap(now, now.Add(time.Hour), c.Peer, c.Hub)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[[16]byte]bool{}
	for _, tc := range []struct {
		raw    []byte
		ttl    time.Duration
		role   uint8
		bearer bool
	}{{wait, WaitTTL, 2, false}, {dial, DialTTL, 1, false}, {boot, time.Hour, 1, true}} {
		kid, got, err := Verify(tc.raw, func(k Kid) (ed25519.PublicKey, bool) { return issuer.PublicKey(), k == issuer.Kid() })
		if err != nil || kid != issuer.Kid() || got.Expires.Sub(now) != tc.ttl || got.Role != tc.role || got.Bearer != tc.bearer || ids[got.JTI] || (got.Bearer && got.JTI != jti) {
			t.Fatalf("issued claims %+v: %v", got, err)
		}
		ids[got.JTI] = true
	}
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIssuer(path); err == nil || string(must(os.ReadFile(path))) != "broken" {
		t.Fatal("replaced invalid key")
	}
}

func TestDeterministicEncoding(t *testing.T) {
	raw := must(Sign(private(), fixture("dial")))
	parts := partsOf(t, raw)
	p := parts[0].([]byte)
	payload := parts[2].([]byte)
	replace := func(b, from, to []byte) []byte {
		t.Helper()
		if !bytes.Contains(b, from) {
			t.Fatal("missing fixture bytes")
		}
		return bytes.Replace(b, from, to, 1)
	}
	check := func(name, stage string, b []byte) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Verify(b, lookup); err == nil || !strings.HasPrefix(err.Error(), stage) {
				t.Fatalf("expected %s, got %v", stage, err)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		from, to []byte
	}{
		{"tag", []byte{0xd2}, []byte{0xd8, 18}},
		{"array", []byte{0x84}, []byte{0x98, 4}},
		{"protected length", []byte{0x4d}, []byte{0x58, 13}},
		{"empty map", []byte{0xa0}, []byte{0xb8, 0}},
		{"payload length", []byte{0x58, 146}, []byte{0x59, 0, 146}},
		{"signature length", []byte{0x58, 64}, []byte{0x59, 0, 64}},
	} {
		check(tc.name, "A2", replace(raw, tc.from, tc.to))
	}
	check("payload integer", "A5", signed(parts, replace(payload, []byte{1, 2}, []byte{1, 0x18, 2})))
	check("field length", "A5", signed(parts, replace(payload, []byte{0x58, 32}, []byte{0x59, 0, 32})))
	reordered := append(append([]byte{payload[0]}, payload[3:]...), payload[1:3]...)
	check("payload order", "A5", signed(parts, reordered))
	parts[2] = reordered
	check("payload order without new signature", "A4", envelope(parts))
	parts[0] = replace(p, []byte{0x27}, []byte{0x38, 7})
	check("protected algorithm", "A2", signed(parts, payload))
	parts[0] = append(append([]byte{0xa2}, p[3:]...), p[1:3]...)
	check("protected order", "A2", signed(parts, payload))
}
func TestExtensionAndTagBoundaries(t *testing.T) {
	parts := partsOf(t, must(Sign(private(), fixture("dial"))))
	payload := parts[2].([]byte)
	extra := append(append([]byte{payload[0] + 1}, payload[1:]...), 0x18, 64)
	for _, tc := range []struct {
		name  string
		value []byte
		valid bool
	}{
		{"core deterministic integer keys", must(enc.Marshal(map[int]int{24: 0, -1: 0})), true},
		{"bytewise integer keys", []byte{0xa2, 0x18, 24, 0, 0x20, 0}, true},
		{"length-first integer keys", []byte{0xa2, 0x20, 0, 0x18, 24, 0}, false},
		{"compound information key", []byte{0xa1, 0x81, 1, 0xd8, 100, 0xf6}, true},
		{"duplicate compound key", []byte{0xa2, 0x81, 1, 0, 0x81, 1, 2}, false},
		{"alternate duplicate encoding", []byte{0xa2, 1, 0, 0x18, 1, 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Verify(signed(parts, append(bytes.Clone(extra), tc.value...)), lookup)
			if (err == nil) != tc.valid {
				t.Fatal(err)
			}
		})
	}
	tag := []byte{0xd9, 0xd9, 0xf7}
	for _, pos := range []int{1, 2} {
		bad := append(bytes.Clone(payload[:pos]), tag...)
		bad = append(bad, payload[pos:]...)
		if _, _, err := Verify(signed(parts, bad), lookup); err == nil {
			t.Fatal("accepted tagged field")
		}
	}
	if _, _, err := Inspect(append(tag, envelope(parts)...)); err == nil {
		t.Fatal("accepted nested COSE tag")
	}
}

func TestInvalidCBORHeadSize(t *testing.T) {
	for _, item := range [][]byte{nil, {0xbf}, {0xff}, {0xbc}, {0x1a, 0}} {
		if _, err := headSize(item); err == nil {
			t.Errorf("accepted head %x", item)
		}
	}
}
