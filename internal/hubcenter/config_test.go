package hubcenter

import (
	"crypto/ed25519"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"remote-enet/internal/hubauth"
	"remote-enet/internal/settings"
)

const sample = `{"version":1,"mode":"center","identity_dir":"identity","listen":"localhost:443","center":{"id":"c","api":["https://example.org/base"]},"node_id":"AAAAAAAAAAAAAAAAAAAAAA","ticket_keys":["AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"]}`

func loadText(t *testing.T, s string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "node.json")
	if err := os.WriteFile(path, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	return Load(path)
}

func TestLoad(t *testing.T) {
	c, err := loadText(t, sample)
	if err != nil || c.SetupLimits != (hubauth.SetupLimits{Concurrent: 64, PerSecond: 32, Burst: 64, TimeoutS: 10}) {
		t.Fatalf("load: %+v, %v", c, err)
	}
	for _, field := range []string{"version", "mode", "identity_dir", "listen", "center", "center.id", "center.api", "node_id", "ticket_keys"} {
		t.Run(field, func(t *testing.T) {
			var m map[string]any
			if err := json.Unmarshal([]byte(sample), &m); err != nil {
				t.Fatal(err)
			}
			if k, ok := strings.CutPrefix(field, "center."); ok {
				delete(m["center"].(map[string]any), k)
			} else {
				delete(m, field)
			}
			b, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := loadText(t, string(b)); err == nil {
				t.Fatal("accepted missing field")
			}
		})
	}
	for _, s := range []string{
		sample + "{}",
		sample + strings.Repeat(" ", 65536),
		sample + "\xff",
		strings.Replace(sample, `"id":"c"`, `"id":"c","id":"d"`, 1),
		strings.Replace(sample, `"api"`, `"API"`, 1),
		strings.Replace(sample, `"version":1`, `"version":"1"`, 1),
		strings.Replace(sample, `"identity"`, "null", 1),
		strings.Replace(sample, `"mode"`, `"clock_skew":30,"mode"`, 1),
		strings.Replace(sample, "https://", "http://", 1),
		strings.Replace(sample, "localhost:443", "bad host:443", 1),
		strings.Replace(sample, `"center",`, `"selfhost",`, 1),
		strings.Replace(sample, "AAAAAA\"", "AAAAA=\"", 1),
	} {
		if _, err := loadText(t, s); err == nil {
			t.Fatalf("accepted invalid config: %.100s", s)
		}
	}
}

func testConfig() Config {
	return Config{Version: 1, Mode: "center", Listen: ":443", IdentityDir: "identity",
		Center:      settings.Center{ID: "c", API: []string{"https://example.org"}},
		SetupLimits: hubauth.SetupLimits{Concurrent: 64, PerSecond: 32, Burst: 64, TimeoutS: 10},
		TicketKeys:  []ed25519.PublicKey{make([]byte, 32)},
	}
}

func TestReloadCompatible(t *testing.T) {
	old := testConfig()
	for _, modify := range []func(*Config){
		func(c *Config) { c.Version++ },
		func(c *Config) { c.Mode = "selfhost" },
		func(c *Config) { c.Listen = ":444" },
		func(c *Config) { c.IdentityDir = "elsewhere" },
		func(c *Config) { c.Center.ID = "other" },
		func(c *Config) { c.Center.API = []string{"https://alt.org"} },
		func(c *Config) { c.NodeID[0]++ },
		func(c *Config) { c.SetupLimits.Concurrent++ },
	} {
		c := old
		modify(&c)
		if c.ReloadCompatible(old) == nil {
			t.Fatalf("accepted change: %+v", c)
		}
	}
	c := old
	c.TicketKeys = []ed25519.PublicKey{ed25519.NewKeyFromSeed(make([]byte, 32)).Public().(ed25519.PublicKey)}
	if err := c.ReloadCompatible(old); err != nil {
		t.Fatal(err)
	}
}
