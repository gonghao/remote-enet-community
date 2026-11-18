package settings

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"remote-enet/internal/identity"
)

const fixture = `{
  "version": 1,
  "role": "operator",
  "allow_link_test": false,
  "client": {"nic_name": "Ethernet 12", "nic_addr": "169.254.12.34", "peer": ""},
  "operator": {
    "listen": "0.0.0.0:47000",
    "endpoint": "home.example:47000",
    "bind": "192.168.1.20",
    "diagnostic_port": 6801,
    "control_port": 6811,
    "discovery_port": 6811,
    "exclusive": true
  }
}`

func writeTestConfig(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertFileUnchanged(t *testing.T, path string, original []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatal("failed save changed the original file")
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".settings-") {
			t.Errorf("temporary file left behind: %s", entry.Name())
		}
	}
}

func TestLoadMissingOnly(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "not-created")
	path := filepath.Join(parent, "settings.json")
	cfg, err := Load(path)
	if err != nil || cfg != Default() {
		t.Fatalf("missing config: %+v, %v", cfg, err)
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("loading missing config changed disk: %v", err)
	}
	for _, path := range []string{"", t.TempDir()} {
		if cfg, err := Load(path); err == nil || cfg != (Config{}) {
			t.Errorf("invalid path %q: %+v, %v", path, cfg, err)
		}
	}
	parentFile := filepath.Join(t.TempDir(), "file")
	writeTestConfig(t, parentFile, []byte("not a directory"))
	if cfg, err := Load(filepath.Join(parentFile, "settings.json")); err == nil || cfg != (Config{}) {
		t.Fatalf("non-directory parent returned defaults: %+v, %v", cfg, err)
	}
}

func TestLoadFixture(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	writeTestConfig(t, path, []byte(fixture+" \n\t"))
	want := Config{
		Version: 1, Role: "operator", AllowLinkTest: false,
		Client: Client{NICName: "Ethernet 12", NICAddr: "169.254.12.34"},
		Operator: Operator{
			Listen: "0.0.0.0:47000", Endpoint: "home.example:47000", Bind: "192.168.1.20",
			DiagnosticPort: 6801, ControlPort: 6811, DiscoveryPort: 6811, Exclusive: true,
		},
	}
	if got, err := Load(path); err != nil || got != want {
		t.Fatalf("fixture = %+v, %v; want %+v", got, err, want)
	}
}

func TestLoadRejectsCorruptConfig(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"whitespace", " \n\t"},
		{"truncated", fixture[:len(fixture)-1]},
		{"garbage", "not JSON"},
		{"bom", "\xef\xbb\xbf" + fixture},
		{"null", "null"},
		{"array", "[]"},
		{"string", `"settings"`},
		{"boolean", "true"},
		{"empty-object", "{}"},
		{"version-only", `{"version":1}`},
		{"unknown-top", strings.Replace(fixture, `"version": 1`, `"unknown": true, "version": 1`, 1)},
		{"unknown-client", strings.Replace(fixture, `"peer": ""`, `"invitation": "secret", "peer": ""`, 1)},
		{"unknown-operator", strings.Replace(fixture, `"exclusive": true`, `"unknown": 1, "exclusive": true`, 1)},
		{"uppercase-top", strings.Replace(fixture, `"version"`, `"Version"`, 1)},
		{"uppercase-client", strings.Replace(fixture, `"nic_name"`, `"NIC_NAME"`, 1)},
		{"uppercase-operator", strings.Replace(fixture, `"exclusive"`, `"EXCLUSIVE"`, 1)},
		{"duplicate-top", strings.Replace(fixture, `"version": 1`, `"version": 2, "version": 1`, 1)},
		{"duplicate-same-value", strings.Replace(fixture, `"allow_link_test": false`, `"allow_link_test": false, "allow_link_test": false`, 1)},
		{"duplicate-escaped-key", strings.Replace(fixture, `"version": 1`, `"\u0076ersion": 1, "version": 1`, 1)},
		{"duplicate-client", strings.Replace(fixture, `"peer": ""`, `"peer": "", "peer": ""`, 1)},
		{"duplicate-object", strings.Replace(fixture, `"operator": {`, `"client": {}, "operator": {`, 1)},
		{"duplicate-operator", strings.Replace(fixture, `"exclusive": true`, `"exclusive": false, "exclusive": true`, 1)},
		{"missing-version", strings.Replace(fixture, `"version": 1,`, "", 1)},
		{"missing-role", strings.Replace(fixture, `"role": "operator",`, "", 1)},
		{"missing-bool", strings.Replace(fixture, `"allow_link_test": false,`, "", 1)},
		{"missing-client-field", strings.Replace(fixture, `"nic_addr": "169.254.12.34",`, "", 1)},
		{"missing-operator-field", strings.Replace(fixture, `"discovery_port": 6811,`, "", 1)},
		{"null-version", strings.Replace(fixture, `"version": 1`, `"version": null`, 1)},
		{"null-role", strings.Replace(fixture, `"role": "operator"`, `"role": null`, 1)},
		{"null-bool", strings.Replace(fixture, `"allow_link_test": false`, `"allow_link_test": null`, 1)},
		{"null-client", strings.Replace(fixture, `"client": {"nic_name": "Ethernet 12", "nic_addr": "169.254.12.34", "peer": ""}`, `"client": null`, 1)},
		{"null-client-field", strings.Replace(fixture, `"peer": ""`, `"peer": null`, 1)},
		{"null-operator", strings.Replace(fixture, `"operator": {`, `"operator": null, "extra": {`, 1)},
		{"null-operator-field", strings.Replace(fixture, `"exclusive": true`, `"exclusive": null`, 1)},
		{"null-port", strings.Replace(fixture, `"control_port": 6811`, `"control_port": null`, 1)},
		{"string-version", strings.Replace(fixture, `"version": 1`, `"version": "1"`, 1)},
		{"fractional-version", strings.Replace(fixture, `"version": 1`, `"version": 1.0`, 1)},
		{"number-bool", strings.Replace(fixture, `"allow_link_test": false`, `"allow_link_test": 0`, 1)},
		{"string-bool", strings.Replace(fixture, `"exclusive": true`, `"exclusive": "true"`, 1)},
		{"array-string", strings.Replace(fixture, `"peer": ""`, `"peer": []`, 1)},
		{"array-object", strings.Replace(fixture, `"client": {"nic_name": "Ethernet 12", "nic_addr": "169.254.12.34", "peer": ""}`, `"client": []`, 1)},
		{"int-overflow", strings.Replace(fixture, `"control_port": 6811`, `"control_port": 999999999999999999999999999999999999`, 1)},
		{"fractional-port", strings.Replace(fixture, `"control_port": 6811`, `"control_port": 6811.1`, 1)},
		{"unsupported-version", strings.Replace(fixture, `"version": 1`, `"version": 2`, 1)},
		{"invalid-role", strings.Replace(fixture, `"role": "operator"`, `"role": "hub"`, 1)},
		{"invalid-port", strings.Replace(fixture, `"control_port": 6811`, `"control_port": 0`, 1)},
		{"equal-tcp-ports", strings.Replace(fixture, `"control_port": 6811`, `"control_port": 6801`, 1)},
		{"invalid-ip", strings.Replace(fixture, `"bind": "192.168.1.20"`, `"bind": "0.0.0.0"`, 1)},
		{"invalid-endpoint", strings.Replace(fixture, `"endpoint": "home.example:47000"`, `"endpoint": "https://home.example:47000"`, 1)},
		{"invalid-peer", strings.Replace(fixture, `"peer": ""`, `"peer": "renet1:secret"`, 1)},
		{"invalid-utf8", strings.Replace(fixture, "Ethernet 12", "Ethernet\xff", 1)},
		{"second-object", fixture + "{}"},
		{"second-null", fixture + "null"},
		{"trailing-garbage", fixture + "x"},
		{"comment", fixture + "// comment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "settings.json")
			original := []byte(tc.data)
			writeTestConfig(t, path, original)
			if cfg, err := Load(path); err == nil || cfg != (Config{}) {
				t.Fatalf("corrupt config returned %+v, %v", cfg, err)
			}
			assertFileUnchanged(t, path, original)
		})
	}
}

func TestLoadSizeLimitIncludesTrailingWhitespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	data := append([]byte(fixture), bytes.Repeat([]byte(" "), maxConfigBytes-len(fixture))...)
	writeTestConfig(t, path, data)
	if _, err := Load(path); err != nil {
		t.Fatalf("rejected config at size limit: %v", err)
	}
	data = append(data, ' ')
	writeTestConfig(t, path, data)
	if cfg, err := Load(path); err == nil || cfg != (Config{}) {
		t.Fatalf("oversized config returned %+v, %v", cfg, err)
	}
}

func TestScopedAddressValidationAndPersistence(t *testing.T) {
	cases := []struct {
		name, value                string
		validListen, validEndpoint bool
	}{
		{"wildcard", "[::]:47000", true, false},
		{"interface-zone", "[fe80::1%eth0]:47000", true, true},
		{"numeric-zone", "[fe80::1%12]:47000", true, true},
		{"printable-zone", "[fe80::1%Ethernet 12]:47000", true, true},
		{"unicode-zone", "[fe80::1%\u7f51\u5361]:47000", true, true},
		{"scoped-wildcard", "[::%eth0]:47000", false, false},
		{"numeric-scoped-wildcard", "[::%12]:47000", false, false},
		{"expanded-scoped-wildcard", "[0:0:0:0:0:0:0:0%eth0]:47000", false, false},
		{"mapped-scoped-wildcard", "[::ffff:0.0.0.0%eth0]:47000", false, false},
		{"lf-zone", "[fe80::1%eth\n0]:47000", false, false},
		{"nul-zone", "[fe80::1%eth\x000]:47000", false, false},
		{"del-zone", "[fe80::1%eth\x7f0]:47000", false, false},
		{"tab-zone", "[fe80::1%eth\t0]:47000", false, false},
		{"cr-zone", "[fe80::1%eth\r0]:47000", false, false},
		{"unicode-control-zone", "[fe80::1%eth\u00850]:47000", false, false},
		{"invalid-utf8-zone", "[fe80::1%eth\xff0]:47000", false, false},
	}
	for _, role := range []string{RoleClient, RoleOperator} {
		for _, field := range []string{"listen", "endpoint"} {
			for _, tc := range cases {
				t.Run(role+"/"+field+"/"+tc.name, func(t *testing.T) {
					cfg := Default()
					cfg.Role = role
					valid := tc.validEndpoint
					originalField := `"endpoint": "home.example:47000"`
					if field == "listen" {
						cfg.Operator.Listen = tc.value
						valid = tc.validListen
						originalField = `"listen": "0.0.0.0:47000"`
					} else {
						cfg.Operator.Endpoint = tc.value
					}
					if err := cfg.Validate(); (err == nil) != valid {
						t.Errorf("Validate: valid=%t, error=%v", valid, err)
					}
					path := filepath.Join(t.TempDir(), "settings.json")
					if valid {
						if err := Save(path, cfg); err != nil {
							t.Fatalf("save valid scoped address: %v", err)
						}
						if got, err := Load(path); err != nil || got != cfg {
							t.Fatalf("scoped address round trip: %+v, %v", got, err)
						}
						return
					}
					valueJSON, err := json.Marshal(tc.value)
					if err != nil {
						t.Fatal(err)
					}
					if !utf8.ValidString(tc.value) {
						// Preserve the invalid byte instead of encoding/json's replacement rune.
						valueJSON = []byte(`"` + tc.value + `"`)
					}
					data := []byte(strings.Replace(fixture, originalField, `"`+field+`": `+string(valueJSON), 1))
					data = bytes.Replace(data, []byte(`"role": "operator"`), []byte(`"role": "`+role+`"`), 1)
					writeTestConfig(t, path, data)
					if got, err := Load(path); err == nil || got != (Config{}) {
						t.Errorf("invalid scoped address loaded: %+v, %v", got, err)
					}
					assertFileUnchanged(t, path, data)
					original := []byte(fixture)
					writeTestConfig(t, path, original)
					if err := Save(path, cfg); err == nil {
						t.Error("saved invalid scoped address")
					}
					assertFileUnchanged(t, path, original)
				})
			}
		}
	}
}

func TestSaveRoundTripAndReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "remote-enet", "settings.json")
	cfg := Default()
	cfg.Role, cfg.AllowLinkTest = RoleOperator, false
	cfg.Client = Client{NICName: "Ethernet adapter \u8f66\u4fa7", NICAddr: "169.254.12.34", Peer: identity.Fingerprint{1, 2, 3}.String()}
	cfg.Operator = Operator{
		Listen: "[::]:47001", Endpoint: "home.example.com:47001", Bind: "192.168.1.20",
		DiagnosticPort: 16801, ControlPort: 16811, DiscoveryPort: 16811, Exclusive: true,
	}
	for _, role := range []string{RoleOperator, RoleClient} {
		cfg.Role = role
		if err := Save(path, cfg); err != nil {
			t.Fatalf("save %s: %v", role, err)
		}
		if got, err := Load(path); err != nil || got != cfg {
			t.Fatalf("round trip %s = %+v, %v; want %+v", role, got, err, cfg)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"version", "role", "allow_link_test", "client", "operator"} {
		if _, ok := fields[name]; !ok {
			t.Errorf("missing snake_case field %s", name)
		}
	}
	if len(fields) != 5 {
		t.Fatalf("unexpected persisted fields: %s", data)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("config permissions: %v, %v", info, err)
		}
		info, err = os.Stat(filepath.Dir(path))
		if err != nil || info.Mode().Perm() != 0o700 {
			t.Fatalf("directory permissions: %v, %v", info, err)
		}
	}
	assertFileUnchanged(t, path, data)
}

func TestSaveUnconfiguredDefaults(t *testing.T) {
	for _, role := range []string{RoleClient, RoleOperator} {
		path := filepath.Join(t.TempDir(), "settings.json")
		cfg := Default()
		cfg.Role = role
		if err := Save(path, cfg); err != nil {
			t.Fatalf("save unconfigured %s: %v", role, err)
		}
		if got, err := Load(path); err != nil || got != cfg {
			t.Fatalf("unconfigured %s round trip = %+v, %v", role, got, err)
		}
	}
}

func TestSaveFailurePreservesOriginal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte(fixture)
	writeTestConfig(t, path, original)
	for _, failure := range []string{"invalid-config", "oversized-config"} {
		t.Run(failure, func(t *testing.T) {
			cfg := Default()
			if failure == "invalid-config" {
				cfg.Operator.Endpoint = "invalid"
			} else {
				cfg.Client.NICName = strings.Repeat("x", maxConfigBytes)
			}
			if err := Save(path, cfg); err == nil {
				t.Fatal("save unexpectedly succeeded")
			}
			assertFileUnchanged(t, path, original)
		})
	}
	if err := Save("", Default()); err == nil {
		t.Fatal("accepted an empty config path")
	}
	parent := filepath.Join(t.TempDir(), "not-created")
	cfg := Default()
	cfg.Version = -1
	if err := Save(filepath.Join(parent, "settings.json"), cfg); err == nil {
		t.Fatal("saved invalid config")
	}
	if _, err := os.Stat(parent); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("validation failure created directories: %v", err)
	}
	if err := Save(filepath.Join(path, "settings.json"), Default()); err == nil {
		t.Fatal("accepted a non-directory parent")
	}
	assertFileUnchanged(t, path, original)
	if err := Save(filepath.Dir(path), Default()); err == nil {
		t.Fatal("replaced a directory with config")
	}
	assertFileUnchanged(t, path, original)
}

func TestSavePermissionFailurePreservesOriginal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permissions are not enforced by Windows chmod")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	original := []byte(fixture)
	writeTestConfig(t, path, original)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	probe, err := os.CreateTemp(dir, "permission-probe-")
	if err == nil {
		probe.Close()
		os.Remove(probe.Name())
		t.Skip("current user can bypass directory permissions")
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	if err := Save(path, Default()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("lost save permission error: %v", err)
	}
	assertFileUnchanged(t, path, original)
	if cfg, err := Load(path); err != nil || cfg.Role != RoleOperator {
		t.Fatalf("original config not readable after failure: %+v, %v", cfg, err)
	}
}

func TestRejectSymlinksIncludingDanglingSymlinks(t *testing.T) {
	dir := t.TempDir()
	target, path := filepath.Join(dir, "target.json"), filepath.Join(dir, "settings.json")
	original := []byte(fixture)
	writeTestConfig(t, target, original)
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("symlink creation requires additional Windows privileges: %v", err)
		}
		t.Fatal(err)
	}
	for _, dangling := range []bool{false, true} {
		if dangling {
			if err := os.Remove(target); err != nil {
				t.Fatal(err)
			}
		}
		if cfg, err := Load(path); err == nil || cfg != (Config{}) {
			t.Errorf("symlink load with dangling=%t: %+v, %v", dangling, cfg, err)
		}
		if err := Save(path, Default()); err == nil {
			t.Errorf("saved through symlink with dangling=%t", dangling)
		}
		if !dangling {
			assertFileUnchanged(t, target, original)
		}
	}
}

func TestDefaultPathsUseOSDirectoryWithoutCreatingFiles(t *testing.T) {
	base := t.TempDir()
	var wantBase string
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", base)
		wantBase = base
	case "darwin", "ios":
		t.Setenv("HOME", base)
		wantBase = filepath.Join(base, "Library", "Application Support")
	default:
		t.Setenv("XDG_CONFIG_HOME", base)
		wantBase = base
	}
	t.Chdir(t.TempDir())
	configPath, stateDir, err := DefaultPaths()
	wantDir := filepath.Join(wantBase, "remote-enet")
	if err != nil || configPath != filepath.Join(wantDir, "settings.json") || stateDir != filepath.Join(wantDir, "state") {
		t.Fatalf("default paths = %q, %q, %v; base %q", configPath, stateDir, err, wantDir)
	}
	if _, err := os.Stat(wantDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default paths created files: %v", err)
	}
}

func TestDefaultPathsFailWithoutOSDirectory(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", "")
	case "darwin", "ios":
		t.Setenv("HOME", "")
	default:
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "")
	}
	if configPath, stateDir, err := DefaultPaths(); err == nil || configPath != "" || stateDir != "" {
		t.Fatalf("missing OS directory returned %q, %q, %v", configPath, stateDir, err)
	}
}

func TestDefaultPathsRejectRelativeOSDirectory(t *testing.T) {
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", "relative")
	case "darwin", "ios":
		t.Setenv("HOME", "relative")
	default:
		t.Setenv("XDG_CONFIG_HOME", "relative")
	}
	if configPath, stateDir, err := DefaultPaths(); err == nil || configPath != "" || stateDir != "" {
		t.Fatalf("relative OS directory returned %q, %q, %v", configPath, stateDir, err)
	}
}
