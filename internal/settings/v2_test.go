package settings

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"remote-enet/internal/identity"
)

func TestValidateHubMode(t *testing.T) {
	fp := identity.Fingerprint{1}.String()
	for _, tc := range []struct {
		mode, addr, fp, field string
	}{
		{"hub", "hub.example:443", fp, ""},
		{"hub", "[::1]:443", strings.ReplaceAll(fp, "-", ""), ""},
		{"hub", "", fp, "operator.hub.addr"},
		{"hub", "https://hub.example:443", fp, "operator.hub.addr"},
		{"hub", "hub.example:0", fp, "operator.hub.addr"},
		{"hub", "hub.example:443", "", "operator.hub.fp"},
		{"hub", "hub.example:443", identity.Fingerprint{1}.Short(), "operator.hub.fp"},
		{"hub", "hub.example:443", identity.Fingerprint{}.String(), "operator.hub.fp"},
		{"direct", "hub.example:443", "", ""},
		{"direct", "invalid", "", "operator.hub.addr"},
		{"direct", "", fp, ""},
		{"direct", "", "invalid", "operator.hub.fp"},
		{"", "", "", "operator.mode"},
	} {
		cfg := Default()
		cfg.Role = RoleOperator
		cfg.Operator.Mode = tc.mode
		cfg.Operator.Hub = Hub{Addr: tc.addr, FP: tc.fp}
		if tc.mode == "hub" {
			cfg.Operator.Listen = "ignored"
		}
		err := cfg.Validate()
		if tc.field == "" && err != nil || tc.field != "" && (err == nil || !strings.Contains(err.Error(), tc.field)) {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

func TestValidateCenter(t *testing.T) {
	const base = "https://center.example/api/"
	for _, tc := range []struct {
		id    string
		api   []string
		field string
	}{
		{"", nil, ""},
		{"a-0.b", []string{base}, ""},
		{strings.Repeat("a", 32), []string{base, base, base, base + strings.Repeat("a", 256-len(base))}, ""},
		{"", []string{base}, "center.id"},
		{"a", nil, "center.api"},
		{"A", []string{base}, "center.id"},
		{strings.Repeat("a", 33), []string{base}, "center.id"},
		{"a", []string{base, base, base, base, base}, "center.api"},
		{"a", []string{base + strings.Repeat("é", (256-len(base))/2+1)}, "center.api[0]"},
		{"a", []string{"http://center.example"}, "center.api[0]"},
		{"a", []string{"//center.example"}, "center.api[0]"},
		{"a", []string{"https:///api"}, "center.api[0]"},
		{"a", []string{"https://user@center.example"}, "center.api[0]"},
		{"a", []string{base + "?"}, "center.api[0]"},
		{"a", []string{base + "?a=b"}, "center.api[0]"},
		{"a", []string{base + "#"}, "center.api[0]"},
		{"a", []string{base + "#fragment"}, "center.api[0]"},
	} {
		for _, role := range []string{RoleClient, RoleOperator} {
			cfg := Default()
			cfg.Role = role
			cfg.Center = Center{ID: tc.id, API: tc.api}
			err := cfg.Validate()
			if tc.field == "" && err != nil || tc.field != "" && (err == nil || !strings.Contains(err.Error(), tc.field)) {
				t.Errorf("%s %+v: %v", role, tc, err)
			}
		}
	}
}

func TestValidateCenterMode(t *testing.T) {
	fp := identity.Fingerprint{1}.String()
	for _, tc := range []struct {
		name   string
		change func(*Config)
		fields []string
	}{
		{"empty inactive fields", func(c *Config) {}, nil},
		{"retained fields", func(c *Config) {
			c.Operator.Listen = ":47000"
			c.Operator.Endpoint = "direct.example:47000"
			c.Operator.Hub = Hub{Addr: "hub.example:443", FP: fp, Share: "public.example:444"}
		}, nil},
		{"missing center", func(c *Config) { c.Center = Center{} }, []string{"center.id", "center.api"}},
		{"missing ID", func(c *Config) { c.Center.ID = "" }, []string{"center.id"}},
		{"missing API", func(c *Config) { c.Center.API = nil }, []string{"center.api"}},
		{"invalid API", func(c *Config) { c.Center.API = []string{"http://center.example"} }, []string{"center.api[0]"}},
		{"invalid listen", func(c *Config) { c.Operator.Listen = "invalid" }, []string{"operator.listen"}},
		{"invalid endpoint", func(c *Config) { c.Operator.Endpoint = "invalid" }, []string{"operator.endpoint"}},
		{"invalid hub address", func(c *Config) { c.Operator.Hub.Addr = "invalid" }, []string{"operator.hub.addr"}},
		{"invalid hub fingerprint", func(c *Config) { c.Operator.Hub.FP = "invalid" }, []string{"operator.hub.fp"}},
		{"invalid hub share", func(c *Config) { c.Operator.Hub.Share = "invalid" }, []string{"operator.hub.share"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Role = RoleOperator
			cfg.Operator.Mode = "center"
			cfg.Operator.Listen = ""
			cfg.Center = Center{ID: "primary", API: []string{"https://center.example"}}
			tc.change(&cfg)
			err := cfg.Validate()
			if len(tc.fields) == 0 && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			for _, field := range tc.fields {
				if err == nil || !strings.Contains(err.Error(), field) {
					t.Errorf("Validate: want error naming %s, got %v", field, err)
				}
			}
		})
	}
}

func TestV2StrictDecodingAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	cfg := Default()
	cfg.Role = RoleOperator
	cfg.Operator.Mode = "hub"
	cfg.Operator.Listen = ""
	cfg.Operator.Hub = Hub{Addr: "hub.example:443", FP: identity.Fingerprint{1}.String(), Share: "public.example:444"}
	cfg.Center = Center{ID: "primary", API: []string{"https://center.example", "https://backup.example"}}
	if err := Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(path); err != nil || !got.Equal(cfg) {
		t.Fatalf("round trip = %+v, %v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ old, replacement, field string }{
		{`"version": 2`, `"version": 3, "future": null`, "unsupported version"},
		{`"mode": "hub",`, "", "operator.mode"},
		{`"hub": {`, `"hub": null, "extra": {`, "operator"},
		{`"addr": "hub.example:443",`, "", "operator.hub.addr"},
		{`"addr":`, `"Addr":`, "operator.hub"},
		{`"addr":`, `"addr": "", "addr":`, "operator.hub"},
		{`"public.example:444"`, `"invalid"`, "operator.hub.share"},
		{`"id": "primary",`, "", "center.id"},
		{`"id":`, `"id": "", "id":`, "center"},
		{`"api": [`, `"api": null, "extra": [`, "center"},
		{`"https://center.example"`, `null`, "center.api[0]"},
		{`"https://center.example"`, `123`, "center.api"},
		{`"https://backup.example"`, `"http://backup.example"`, "center.api[1]"},
	} {
		bad := strings.Replace(string(data), tc.old, tc.replacement, 1)
		if bad == string(data) {
			t.Fatalf("missing fixture field %s", tc.old)
		}
		writeTestConfig(t, path, []byte(bad))
		if got, err := Load(path); err == nil || !strings.Contains(err.Error(), tc.field) || !got.Equal(Config{}) {
			t.Errorf("%s replaced with %s: %+v, %v", tc.old, tc.replacement, got, err)
		}
	}
}

func TestLoadBuildDefaultCenter(t *testing.T) {
	original := defaultCenterJSON
	t.Cleanup(func() { defaultCenterJSON = original })
	// Simulate -ldflags '-X remote-enet/internal/settings.defaultCenterJSON={"id":"official","api":["https://center.example"]}'.
	defaultCenterJSON = `{"id":"official","api":["https://center.example"]}`
	wantDefault := baseConfig()
	wantDefault.Operator.Mode = "center"
	wantDefault.Center = Center{ID: "official", API: []string{"https://center.example"}}
	if got := Default(); !got.Equal(wantDefault) {
		t.Fatalf("Default with a build center: %+v", got)
	}
	dir := t.TempDir()
	legacy := filepath.Join(dir, "v1.json")
	writeTestConfig(t, legacy, []byte(fixture))
	missing := filepath.Join(dir, "missing.json")
	if got, err := Load(missing); err != nil || !got.Equal(wantDefault) {
		t.Fatalf("missing config with a build center: %+v, %v", got, err)
	}
	migrated, err := Load(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if migrated.Version != Version || migrated.Operator.Mode != "direct" || migrated.Operator.Hub != (Hub{}) ||
		migrated.Center.ID != wantDefault.Center.ID || !slices.Equal(migrated.Center.API, wantDefault.Center.API) {
		t.Fatalf("v1 migration with a build center: %+v", migrated)
	}
	assertFileUnchanged(t, legacy, []byte(fixture))
	if err := Save(legacy, migrated); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(legacy)
	if err != nil || !strings.Contains(string(saved), `"version": 2`) {
		t.Fatalf("migration was not saved as v2: %s, %v", saved, err)
	}
	explicit := filepath.Join(dir, "v2.json")
	hub := baseConfig()
	hub.Operator.Mode = "hub"
	hub.Operator.Hub = Hub{Addr: "hub.example:443", FP: identity.Fingerprint{1}.String()}
	for _, cfg := range []Config{baseConfig(), hub, wantDefault} {
		if err := Save(explicit, cfg); err != nil {
			t.Fatal(err)
		}
		if got, err := Load(explicit); err != nil || !got.Equal(cfg) {
			t.Fatalf("existing v2 %s config changed: %+v, %v", cfg.Operator.Mode, got, err)
		}
	}
	writeTestConfig(t, legacy, []byte(fixture))
	for _, invalid := range []string{`{"id":"INVALID","api":["https://center.example"]}`, `{"id":"official","api":[null]}`} {
		defaultCenterJSON = invalid
		for _, path := range []string{legacy, explicit, missing} {
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default center") {
				t.Errorf("invalid build defaults for %s: %v", path, err)
			}
		}
	}
}

func TestVersionSpecificFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	check := func(data []byte, message string) {
		t.Helper()
		writeTestConfig(t, path, data)
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("expected %q, got %v", message, err)
		}
	}
	for _, tc := range []struct{ before, field, value string }{
		{`"version":`, "center", `{"id":"","api":[]}`},
		{`"listen":`, "hub", `{"addr":"","fp":""}`},
		{`"listen":`, "mode", `"direct"`},
	} {
		field := `"` + tc.field + `"`
		check([]byte(strings.Replace(fixture, tc.before, field+":"+tc.value+","+tc.before, 1)), "unknown field "+field)
	}
	if err := Save(path, Default()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	delete(fields, "center")
	data, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	check(data, "config.center: missing required field")
}
