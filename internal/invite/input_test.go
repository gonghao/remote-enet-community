package invite

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseInput(t *testing.T) {
	const code = "00001234"
	for _, tc := range []struct {
		name, input, address string
		kind                 InputKind
		err                  error
	}{
		{name: "leading zeros", input: "192.0.2.1:00080 " + code, address: "192.0.2.1:80", kind: Direct},
		{name: "Unicode whitespace and full-width digits", input: "\u3000Example.NET.:47000\n００００\t１２３４\u00a0", address: "example.net.:47000", kind: Direct},
		{name: "single label", input: "MY-HUB:65535 " + code, address: "my-hub:65535", kind: Direct},
		{name: "leading BOM", input: "\ufeff\u3000host:1 " + code, address: "host:1", kind: Direct},
		{name: "double BOM", input: "\ufeff\ufeffhost:1 " + code, err: ErrInvalidHost},
		{name: "IPv6", input: "[2001:0DB8::2]:47000 0000 1234", address: "[2001:db8::2]:47000", kind: Direct},
		{name: "center", input: code, kind: Center},
		{name: "grouped center", input: "００００\u3000１２３４", kind: Center},
		{name: "unbracketed IPv6", input: "2001:db8::2:47000 " + code, err: ErrInvalidHost},
		{name: "zone", input: "[fe80::1%eth0]:47000 " + code, err: ErrInvalidHost},
		{name: "invalid IPv4", input: "192.000.2.1:47000 " + code, err: ErrInvalidHost},
		{name: "numeric dotted", input: "999.999.:47000 " + code, err: ErrInvalidHost},
		{name: "numeric single label", input: "12345678:47000 " + code, err: ErrInvalidHost},
		{name: "unspecified IPv4", input: "0.0.0.0:47000 " + code, err: ErrInvalidHost},
		{name: "unspecified IPv6", input: "[::]:47000 " + code, err: ErrInvalidHost},
		{name: "mapped unspecified", input: "[::ffff:0.0.0.0]:47000 " + code, err: ErrInvalidHost},
		{name: "bracketed DNS", input: "[host]:47000 " + code, err: ErrInvalidHost},
		{name: "empty host", input: ":47000 " + code, err: ErrInvalidHost},
		{name: "URL", input: "https://host:47000 " + code, err: ErrInvalidHost},
		{name: "userinfo", input: "user@host:47000 " + code, err: ErrInvalidHost},
		{name: "path", input: "host:47000/path " + code, err: ErrInvalidPort},
		{name: "DNS label length", input: strings.Repeat("a", 64) + ":47000 " + code, err: ErrInvalidHost},
		{name: "DNS total length", input: strings.Repeat(strings.Repeat("a", 63)+".", 4) + ":47000 " + code, err: ErrInvalidHost},
		{name: "DNS hyphen", input: "host-.example:47000 " + code, err: ErrInvalidHost},
		{name: "empty port", input: "host: " + code, err: ErrInvalidPort},
		{name: "zero port", input: "host:0 " + code, err: ErrInvalidPort},
		{name: "overflow port", input: "host:65536 " + code, err: ErrInvalidPort},
		{name: "signed port", input: "host:+1 " + code, err: ErrInvalidPort},
		{name: "full-width port", input: "host:１ " + code, err: ErrInvalidPort},
		{name: "extra field", input: "host:1 0000 1234 extra", err: ErrInvalidCode},
		{name: "extra digits", input: "host:1 " + code + " 1", err: ErrInvalidCode},
		{name: "zero-width", input: "host:1 " + code + "\u200b", err: ErrInvalidCode},
		{name: "hyphenated code", input: "my-hub:1 0000-1234", address: "my-hub:1", kind: Direct},
		{name: "hyphenated center", input: "0000-1234", kind: Center},
		{name: "other digits", input: "host:1 ٠٠٠٠١٢٣٤", err: ErrInvalidCode},
		{name: "bad raw invite", input: "RENET1:" + code, err: ErrInvalidInvite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseInput(tc.input)
			if !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if err != nil {
				if strings.Contains(err.Error(), code) || strings.Contains(err.Error(), tc.input) || got.Kind != 0 || got.Code != "" {
					t.Fatal("failed parse leaked input or returned usable data")
				}
				return
			}
			wantCode := ""
			if tc.kind == Direct {
				wantCode = code
			}
			if got.Kind != tc.kind || got.Address != tc.address || got.Code != wantCode {
				t.Fatal("unexpected parsed input")
			}
		})
	}
}

func TestParseRawInput(t *testing.T) {
	i := Invite{Version: 1, Token: [16]byte{42}, ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Second)}
	raw, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{raw, strings.ToUpper(raw[:7]) + strings.ToLower(raw[7:])} {
		got, err := ParseInput("\ufeff\u3000" + s + "\n")
		if err != nil || got.Kind != Raw || got.Invite.Token != i.Token || !got.Invite.ExpiresAt.Equal(i.ExpiresAt) {
			t.Fatal("raw invitation did not round trip")
		}
	}
}

func TestParseInputExpiredInvite(t *testing.T) {
	i := Invite{Version: 1, Token: [16]byte{42}, ExpiresAt: time.Unix(100, 0)}
	raw, err := i.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseInput(raw)
	if !errors.Is(err, ErrInvalidInvite) || !errors.Is(err, ErrInviteExpired) || err.Error() != "Invitation expired" {
		t.Fatal("expired invitation did not return the fixed expiry error")
	}
	if got.Kind != 0 || got.Invite.Token != [16]byte{} {
		t.Fatal("expired invitation returned usable data")
	}
	if _, err := ParseInput(raw + "!"); err != ErrInvalidInvite {
		t.Fatal("malformed invitation did not return the generic error")
	}
}

func TestInputErrorLocaleKeys(t *testing.T) {
	for _, locale := range []string{"en", "zh-Hans", "zh-Hant"} {
		t.Run(locale, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("..", "locales", locale+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var catalog map[string]string
			if err := json.Unmarshal(data, &catalog); err != nil {
				t.Fatal(err)
			}
			for _, err := range []error{ErrInvalidInvite, ErrInviteExpired, ErrInvalidHost, ErrInvalidPort, ErrInvalidCode} {
				if _, ok := catalog[err.Error()]; !ok {
					t.Errorf("missing locale key %q", err.Error())
				}
			}
		})
	}
}
