package hubauth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoad(t *testing.T) {
	f := setup(t)
	path := filepath.Join(t.TempDir(), "hub.json")
	load := func(s string) (Config, error) {
		t.Helper()
		if err := os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		return Load(path)
	}
	issuer, _ := json.Marshal(f.cfg.Issuers)
	base := fmt.Sprintf(`{"version":1,"mode":"selfhost","identity_dir":%q,"listen":":443","issuers":%s}`, f.cfg.IdentityDir, issuer)
	c, err := load(base + strings.Repeat(" ", (64<<10)-len(base)))
	if err != nil {
		t.Fatal(err)
	}
	if c.ClockSkewS != 30 || c.TicketMaxTTLS != 2678400 || c.BootstrapMaxTTLS != 86400 || c.PairingWindowMaxS != 900 || c.SetupLimits != (SetupLimits{64, 32, 64, 10}) || c.PairingOperator != "" {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	for _, tc := range []struct{ old, new, path string }{
		{`"version":1,`, "", "version"},
		{`"mode":"selfhost",`, "", "mode"},
		{fmt.Sprintf(`"identity_dir":%q,`, c.IdentityDir), "", "identity_dir"},
		{`"listen":":443",`, "", "listen"},
		{fmt.Sprintf(`,"issuers":%s`, issuer), "", "issuers"},
		{`"version":1`, `"Version":1`, "Version"},
		{`"version":1`, `"version":1,"version":1`, "version"},
		{`"version":1`, `"version":"1"`, "version"},
		{`"version":1`, `"version":1.0`, "version"},
		{`"name":""`, `"name":null`, "issuers[0].name"},
		{`"name":""`, `"name":"","name":""`, "issuers[0].name"},
		{`"name":""`, `"extra":true`, "issuers[0].extra"},
		{`"version":1`, `"version":1,"deny":{"devices":[null]}`, "deny.devices[0]"},
		{`"version":1`, `"version":1,"setup_limits":null`, "setup_limits"},
		{string(issuer), `[false]`, "issuers[0]"},
	} {
		_, err := load(strings.Replace(base, tc.old, tc.new, 1))
		if err == nil || !strings.Contains(err.Error(), tc.path) {
			t.Errorf("%s => %s: %v", tc.old, tc.new, err)
		}
	}
	for _, s := range []string{base + "{}", base + "?", base + strings.Repeat(" ", 65537-len(base)), strings.Replace(base, "selfhost", "\xff", 1), "{", "null"} {
		if _, err := load(s); err == nil {
			t.Error("invalid configuration accepted")
		}
	}
	c, err = load(strings.Replace(base, `"version":1`, `"version":1,"setup_limits":{"burst":2}`, 1))
	if err != nil || c.SetupLimits != (SetupLimits{64, 32, 2, 10}) {
		t.Fatalf("partial defaults: %+v, %v", c.SetupLimits, err)
	}
}

func TestValidate(t *testing.T) {
	f := setup(t)
	for _, tc := range []struct {
		field  string
		change func(*Config)
	}{
		{"version", func(c *Config) { c.Version = 2 }},
		{"mode", func(c *Config) { c.Mode = "other" }},
		{"identity_dir", func(c *Config) { c.IdentityDir = t.TempDir() }},
		{"listen", func(c *Config) { c.Listen = "bad host:443" }},
		{"issuers", func(c *Config) { c.Issuers = nil }},
		{"issuers", func(c *Config) { c.Issuers = make([]Issuer, 65) }},
		{"issuers[1].public_key", func(c *Config) { c.Issuers = append(c.Issuers, c.Issuers[0]) }},
		{"issuers[0].public_key", func(c *Config) { c.Issuers[0].PublicKey += "=" }},
		{"issuers[0].operator", func(c *Config) { c.Issuers[0].Operator = "" }},
		{"issuers[0].name", func(c *Config) { c.Issuers[0].Name = "\n" }},
		{"issuers[0].name", func(c *Config) { c.Issuers[0].Name = strings.Repeat("x", 65) }},
		{"pairing_operator", func(c *Config) { c.PairingOperator = f.h.Fingerprint().String() }},
		{"setup_limits.concurrent", func(c *Config) { c.SetupLimits.Concurrent = 0 }},
		{"setup_limits.per_second", func(c *Config) { c.SetupLimits.PerSecond = -1 }},
		{"setup_limits.burst", func(c *Config) { c.SetupLimits.Burst = 0 }},
		{"deny.devices", func(c *Config) { c.Deny.Devices = make([]string, 10001) }},
		{"deny.jtis", func(c *Config) { c.Deny.JTIs = make([]string, 10001) }},
		{"deny.devices[0]", func(c *Config) { c.Deny.Devices = []string{"bad"} }},
		{"deny.jtis[0]", func(c *Config) { c.Deny.JTIs = []string{strings.Repeat("A", 20) + "\n\n"} }},
	} {
		c := f.cfg
		c.Issuers = append([]Issuer(nil), c.Issuers...)
		tc.change(&c)
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v", tc.field, err)
		}
	}
	for _, tc := range []struct {
		field  string
		lo, hi int64
		set    func(*Config, int64)
	}{
		{"clock_skew_s", 5, 300, func(c *Config, v int64) { c.ClockSkewS = v }},
		{"ticket_max_ttl_s", 2592000, 31536000, func(c *Config, v int64) { c.TicketMaxTTLS = v }},
		{"bootstrap_max_ttl_s", 300, 86400, func(c *Config, v int64) { c.BootstrapMaxTTLS = v }},
		{"pairing_window_max_s", 300, 3600, func(c *Config, v int64) { c.PairingWindowMaxS = v }},
		{"setup_limits.timeout_s", 1, 30, func(c *Config, v int64) { c.SetupLimits.TimeoutS = int(v) }},
	} {
		for _, n := range []int64{tc.lo - 1, tc.lo, tc.hi, tc.hi + 1} {
			c := f.cfg
			tc.set(&c, n)
			err := c.Validate()
			if n < tc.lo || n > tc.hi {
				if err == nil || !strings.Contains(err.Error(), tc.field) {
					t.Errorf("%s=%d: %v", tc.field, n, err)
				}
			} else if err != nil {
				t.Error(err)
			}
		}
	}
}

func TestReloadCompatible(t *testing.T) {
	c := setup(t).cfg
	for _, tc := range []struct {
		field  string
		change func(*Config)
	}{{"identity_dir", func(c *Config) { c.IdentityDir += "2" }},
		{"listen", func(c *Config) { c.Listen = ":444" }},
		{"mode", func(c *Config) { c.Mode = "other" }},
		{"setup_limits", func(c *Config) { c.SetupLimits.Concurrent++ }}} {
		next := c
		tc.change(&next)
		if err := next.ReloadCompatible(c); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v", tc.field, err)
		}
	}
	next := c
	next.PairingOperator = ""
	next.ClockSkewS++
	if err := next.ReloadCompatible(c); err != nil {
		t.Fatal(err)
	}
}

func TestIdentityValidationIsReadOnly(t *testing.T) {
	f := setup(t)
	cert := filepath.Join(f.cfg.IdentityDir, "device.crt")
	if err := os.Remove(cert); err != nil {
		t.Fatal(err)
	}
	if err := f.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cert); !os.IsNotExist(err) {
		t.Fatalf("configuration check recreated certificate: %v", err)
	}
}
