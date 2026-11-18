package desktopui

import (
	"bytes"
	"flag"
	"fmt"
	"path/filepath"
	"testing"
)

func TestOptionsUseStableUserPathsAndExplicitOverrides(t *testing.T) {
	var out bytes.Buffer
	defaults, err := parseOptions("client", nil, &out)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(defaults.configPath) || !filepath.IsAbs(defaults.stateDir) {
		t.Fatalf("paths must be absolute: %+v", defaults)
	}
	config, state := filepath.Join(t.TempDir(), "gui.json"), filepath.Join(t.TempDir(), "identity")
	got, err := parseOptions("client", []string{"--config", config, "--state-dir", state, "--version"}, &out)
	if err != nil || got.configPath != config || got.stateDir != state || !got.version {
		t.Fatalf("override: %+v, %v", got, err)
	}
}

func TestOptionsRejectUnexpectedArguments(t *testing.T) {
	for _, args := range [][]string{{"operator"}, {"--config", ""}, {"--state-dir", ""}, {"--unknown"}} {
		if _, err := parseOptions("client", args, &bytes.Buffer{}); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	if _, err := parseOptions("client", []string{"--help"}, &bytes.Buffer{}); err != flag.ErrHelp {
		t.Fatalf("help: %v", err)
	}
}

func TestExecutablesRejectRuntimeRoleSelection(t *testing.T) {
	for _, role := range []string{"client", "operator"} {
		for _, args := range [][]string{{"--role", "client"}, {"--role", "operator"}, {"client"}, {"operator"}} {
			if _, err := parseOptions(role, args, &bytes.Buffer{}); err == nil {
				t.Fatalf("%s accepted runtime role selection %q", role, args)
			}
		}
	}
	client, _ := productForRole("client")
	operator, _ := productForRole("operator")
	if client.name == operator.name || client.command == operator.command {
		t.Fatal("the applications share an identity or executable name")
	}
}

func TestHelpVersionAndExplicitPathsDoNotNeedDefaultProfile(t *testing.T) {
	calls := 0
	unavailable := func(string) (string, string, error) { calls++; return "", "", fmt.Errorf("profile unavailable") }
	for _, role := range []string{"client", "operator"} {
		if _, err := parseOptionsWithPaths(role, []string{"--help"}, &bytes.Buffer{}, unavailable); err != flag.ErrHelp {
			t.Fatal(err)
		}
		got, err := parseOptionsWithPaths(role, []string{"--version"}, &bytes.Buffer{}, unavailable)
		if err != nil || !got.version {
			t.Fatalf("version: %v", err)
		}
		_, err = parseOptionsWithPaths(role, []string{"--config", filepath.Join(t.TempDir(), "gui.json"), "--state-dir", t.TempDir()}, &bytes.Buffer{}, unavailable)
		if err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("non-profile actions resolved the default user directory")
	}
	if _, err := parseOptionsWithPaths("operator", nil, &bytes.Buffer{}, unavailable); err == nil || calls != 1 {
		t.Fatal("a missing required default profile was ignored")
	}
}
