package settings

import (
	"os"
	"path/filepath"
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
		{"direct", "hub.example:443", "", "operator.hub.addr"},
		{"direct", "", fp, "operator.hub.fp"},
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

func TestV2StrictDecodingAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	cfg := Default()
	cfg.Role = RoleOperator
	cfg.Operator.Mode = "hub"
	cfg.Operator.Listen = ""
	cfg.Operator.Hub = Hub{Addr: "hub.example:443", FP: identity.Fingerprint{1}.String()}
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
	if got := Default(); got.Center.ID != "official" {
		t.Fatalf("Default lost the build center: %+v", got.Center)
	}
	dir := t.TempDir()
	legacy := filepath.Join(dir, "v1.json")
	writeTestConfig(t, legacy, []byte(fixture))
	for _, path := range []string{legacy, filepath.Join(dir, "missing.json")} {
		got, err := Load(path)
		if err != nil || got.Version != Version || got.Operator.Mode != "direct" || got.Operator.Hub != (Hub{}) ||
			got.Center.ID != "official" || len(got.Center.API) != 1 || got.Center.API[0] != "https://center.example" {
			t.Fatalf("build defaults for %s: %+v, %v", path, got, err)
		}
	}
	assertFileUnchanged(t, legacy, []byte(fixture))
	migrated, err := Load(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(legacy, migrated); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(legacy)
	if err != nil || !strings.Contains(string(saved), `"version": 2`) {
		t.Fatalf("migration was not saved as v2: %s, %v", saved, err)
	}
	explicit := filepath.Join(dir, "v2.json")
	if err := Save(explicit, baseConfig()); err != nil {
		t.Fatal(err)
	}
	if got, err := Load(explicit); err != nil || !got.Equal(baseConfig()) {
		t.Fatalf("explicit empty center replaced: %+v, %v", got, err)
	}
	writeTestConfig(t, legacy, []byte(fixture))
	for _, invalid := range []string{`{"id":"INVALID","api":["https://center.example"]}`, `{"id":"official","api":[null]}`} {
		defaultCenterJSON = invalid
		for _, path := range []string{legacy, explicit, filepath.Join(dir, "missing.json")} {
			if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "default center") {
				t.Errorf("invalid build defaults for %s: %v", path, err)
			}
		}
	}
}
