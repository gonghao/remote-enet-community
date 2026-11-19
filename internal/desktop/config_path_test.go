package desktop

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"remote-enet/internal/settings"
)

func TestFixedRoleMissingPathMustUseLoader(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			opts := Options{FixedRole: role, ConfigPath: filepath.Join(t.TempDir(), "missing", "settings.json")}
			for _, cause := range []error{os.ErrPermission, io.ErrUnexpectedEOF} {
				called := false
				cfg, err := loadConfig(opts, func(path string) (settings.Config, error) {
					called = true
					if path != opts.ConfigPath {
						t.Fatalf("loader path = %q, want %q", path, opts.ConfigPath)
					}
					return settings.Default(), &os.PathError{Op: "lstat", Path: path, Err: cause}
				})
				if !called || !errors.Is(err, cause) || !cfg.Equal(settings.Config{}) {
					t.Fatalf("fixed role bypassed load failure %v: called=%t, config=%+v, err=%v", cause, called, cfg, err)
				}
			}
		})
	}
}

func TestFixedRoleConfigurationPathClassification(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			for _, tc := range []struct {
				name, suffix string
				parentFile   bool
			}{
				{"missing-file", "settings.json", false},
				{"missing-parent", filepath.Join("missing", "settings.json"), false},
				{"missing-ancestors", filepath.Join("missing", "nested", "settings.json"), false},
				{"file-parent", "settings.json", true},
				{"file-ancestor", filepath.Join("missing", "nested", "settings.json"), true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					dir := t.TempDir()
					parent := dir
					if tc.parentFile {
						parent = filepath.Join(dir, "file")
						if err := os.WriteFile(parent, []byte("not a directory"), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					opts := Options{FixedRole: role, ConfigPath: filepath.Join(parent, tc.suffix), StateDir: filepath.Join(dir, "state")}
					cfg, err := loadConfig(opts, settings.Load)
					if tc.parentFile {
						if err == nil || !cfg.Equal(settings.Config{}) {
							t.Fatalf("non-directory ancestor returned %+v, %v", cfg, err)
						}
						c, err := New(t.Context(), opts)
						if c != nil {
							_ = c.Close(t.Context())
						}
						if err == nil || c != nil {
							t.Fatalf("New accepted a non-directory ancestor: %v", err)
						}
						if raw, err := os.ReadFile(parent); err != nil || string(raw) != "not a directory" {
							t.Fatalf("New changed the parent file: %q, %v", raw, err)
						}
					} else {
						want := settings.Default()
						want.Role = role
						if err != nil || !cfg.Equal(want) {
							t.Fatalf("missing fixed-role config returned %+v, %v", cfg, err)
						}
						if c := fixedController(t, opts, production()); !c.Snapshot().Config.Equal(want) {
							t.Fatal("New did not use fixed-role defaults")
						}
					}
					entries, err := os.ReadDir(dir)
					if err != nil {
						t.Fatal(err)
					}
					wantEntries := 0
					if tc.parentFile {
						wantEntries = 1
					}
					if len(entries) != wantEntries {
						t.Fatalf("New changed disk: %v", entries)
					}
				})
			}
		})
	}
}

func TestFixedRoleRejectsSymlinkConfigurationPaths(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		for _, kind := range []string{"config", "dangling-config", "file-parent", "dangling-parent", "loop-parent"} {
			t.Run(role+"/"+kind, func(t *testing.T) {
				dir := t.TempDir()
				target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
				path := link
				switch kind {
				case "config", "file-parent":
					cfg := settings.Default()
					cfg.Role = role
					if err := settings.Save(target, cfg); err != nil {
						t.Fatal(err)
					}
				case "loop-parent":
					target = link
				}
				if kind != "config" && kind != "dangling-config" {
					path = filepath.Join(link, "missing", "settings.json")
				}
				if err := os.Symlink(target, link); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlink creation requires additional Windows privileges: %v", err)
					}
					t.Fatal(err)
				}
				opts := Options{FixedRole: role, ConfigPath: path, StateDir: filepath.Join(dir, "state")}
				if cfg, err := loadConfig(opts, settings.Load); err == nil || !cfg.Equal(settings.Config{}) {
					t.Fatalf("symlink configuration returned %+v, %v", cfg, err)
				}
				if got, err := os.Readlink(link); err != nil || got != target {
					t.Fatalf("load changed symlink: %q, %v", got, err)
				}
			})
		}
	}
}
