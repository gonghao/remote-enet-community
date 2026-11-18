package settings

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDefaultPathsForRoleKeepSeparateConfigsAndSharedState(t *testing.T) {
	base := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", base)
	case "darwin", "ios":
		t.Setenv("HOME", base)
	default:
		t.Setenv("XDG_CONFIG_HOME", base)
	}
	t.Chdir(t.TempDir())
	sharedConfig, sharedState, err := DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(sharedConfig)
	for _, role := range []string{RoleClient, RoleOperator} {
		configPath, stateDir, err := DefaultPathsForRole(role)
		if err != nil || configPath != filepath.Join(root, role, "settings.json") || stateDir != sharedState {
			t.Fatalf("role paths: %q, %q, %v", configPath, stateDir, err)
		}
	}
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("path resolution created directories: %v", err)
	}
	if afterConfig, afterState, err := DefaultPaths(); err != nil || afterConfig != sharedConfig || afterState != sharedState {
		t.Fatal("role paths changed generic paths")
	}
}

func TestDefaultPathsForRoleRejectUnknownRoles(t *testing.T) {
	for _, role := range []string{"", "Client", "../operator", "client/operator", "unknown"} {
		if configPath, stateDir, err := DefaultPathsForRole(role); err == nil || configPath != "" || stateDir != "" {
			t.Fatalf("unknown role %q returned %q, %q, %v", role, configPath, stateDir, err)
		}
	}
}
