package settings

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadPathClassification(t *testing.T) {
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
				writeTestConfig(t, parent, []byte("not a directory"))
			}
			path := filepath.Join(parent, tc.suffix)
			_, statErr := os.Lstat(path)
			var pathErr *os.PathError
			if errors.As(statErr, &pathErr) {
				t.Logf("Lstat: %v; errno: %#v; not-exist: %t", statErr, pathErr.Err, errors.Is(statErr, os.ErrNotExist))
			} else {
				t.Fatalf("expected a path error, got %v", statErr)
			}
			// Exercise the Windows fallback even where Lstat returns ENOTDIR.
			if err := checkMissingConfigParents(path); (err == nil) == tc.parentFile {
				t.Fatalf("parent classification with parentFile=%t: %v", tc.parentFile, err)
			}
			cfg, err := Load(path)
			if tc.parentFile {
				if err == nil || cfg != (Config{}) {
					t.Fatalf("non-directory ancestor returned %+v, %v", cfg, err)
				}
				assertFileUnchanged(t, parent, []byte("not a directory"))
			} else if err != nil || cfg != Default() {
				t.Fatalf("missing config returned %+v, %v", cfg, err)
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
				t.Fatalf("Load changed disk: %v", entries)
			}
		})
	}
}

func TestLoadMissingRelativeAncestors(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	path := filepath.Join("missing", "nested", "settings.json")
	if cfg, err := Load(path); err != nil || cfg != Default() {
		t.Fatalf("missing relative config returned %+v, %v", cfg, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Fatalf("Load changed disk: %v, %v", entries, err)
	}
}

func TestLoadSymlinkParents(t *testing.T) {
	for _, kind := range []string{"directory", "file", "dangling", "loop"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			target, link := filepath.Join(dir, "target"), filepath.Join(dir, "link")
			switch kind {
			case "directory":
				if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
			case "file":
				writeTestConfig(t, target, []byte("not a directory"))
			case "loop":
				target = link
			}
			if err := os.Symlink(target, link); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink creation requires additional Windows privileges: %v", err)
				}
				t.Fatal(err)
			}
			suffixes := []string{"settings.json", filepath.Join("missing", "nested", "settings.json")}
			if runtime.GOOS != "windows" {
				// Unix resolves dot segments during traversal, not before it.
				suffixes = append(suffixes, ".."+string(os.PathSeparator)+"settings.json")
			}
			for _, suffix := range suffixes {
				cfg, err := Load(link + string(os.PathSeparator) + suffix)
				if kind == "directory" {
					if err != nil || cfg != Default() {
						t.Fatalf("valid directory symlink with suffix %q returned %+v, %v", suffix, cfg, err)
					}
					if entries, err := os.ReadDir(target); err != nil || len(entries) != 0 {
						t.Fatalf("Load changed symlink target: %v, %v", entries, err)
					}
				} else if err == nil || cfg != (Config{}) {
					t.Fatalf("invalid symlink ancestor with suffix %q returned %+v, %v", suffix, cfg, err)
				}
			}
		})
	}
}

func TestLoadUnsearchableParent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix directory permissions are not enforced by Windows chmod")
	}
	dir := filepath.Join(t.TempDir(), "blocked")
	if err := os.Mkdir(dir, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Error(err)
		}
	})
	path := filepath.Join(dir, "missing", "settings.json")
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrPermission) {
		if errors.Is(err, os.ErrNotExist) {
			t.Skip("current user can bypass directory permissions")
		}
		t.Fatalf("permission probe: %v", err)
	}
	if cfg, err := Load(path); !errors.Is(err, os.ErrPermission) || cfg != (Config{}) {
		t.Fatalf("lost load permission error: %+v, %v", cfg, err)
	}
}
