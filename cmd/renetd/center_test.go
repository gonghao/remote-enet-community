package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"remote-enet/internal/settings"
)

func TestCenterFlagsExclusive(t *testing.T) {
	for _, args := range [][]string{
		{"operator", "--center-node", "node", "--listen", "localhost:47000", "--bind", "127.0.0.1"},
		{"operator", "--center-node", "node", "--share-address", "localhost:47000", "--bind", "127.0.0.1"},
		{"client", "--center-code-file", "absent", "--pair-file", "also-absent"},
		{"client", "--center-code-file", "absent", "--invite", "invitation"},
		{"client", "--center-code-file", "absent", "--peer", "operator"},
	} {
		err := run(context.Background(), append(args, "--state-dir", t.TempDir()), io.Discard, io.Discard)
		if err == nil || !(strings.Contains(err.Error(), "mutually exclusive") || strings.Contains(err.Error(), "exactly one")) {
			t.Errorf("%v: %v", args, err)
		}
	}
}

func TestCenterCodeFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "code")
	for _, v := range []struct{ input, want string }{
		{"００１２ ３４５６\n", "00123456"},
		{strings.Repeat(" ", 4088) + "12345678", "12345678"},
		{strings.Repeat(" ", 4089) + "12345678", ""},
		{"123x5678", ""},
	} {
		if err := os.WriteFile(path, []byte(v.input), 0600); err != nil {
			t.Fatal(err)
		}
		raw, err := readCenterCodeFile(path)
		if len(v.input) > 4096 && err == nil {
			t.Fatal("oversize accepted")
		}

		if raw != v.want || (err == nil) != (v.want != "") {
			t.Fatal("wrong code or size handling")
		}
	}
}

func TestCenterAnswer(t *testing.T) {
	if !sasMatches("００1 ２３４", "001234") || sasMatches("01234", "001234") || sasMatches("001235", "001234") {
		t.Fatal("operator comparison")
	}
	for _, s := range []string{"", "123456", strings.Repeat(" ", 129) + "\n"} {
		if copyAnswer(strings.NewReader(s), io.Discard) == nil {
			t.Fatal("accepted incomplete or oversized input")
		}
	}
	var out bytes.Buffer
	if err := copyAnswer(strings.NewReader("\r\nno\n"), &out); err != nil || out.Len() != 0 {
		t.Fatal("client confirmation")
	}
}

func TestCenterOperatorLoginHint(t *testing.T) {
	cfg := settings.Default()
	cfg.Center = settings.Center{ID: "test-center", API: []string{"https://127.0.0.1"}}
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := settings.Save(path, cfg); err != nil {
		t.Fatal(err)
	}
	err := run(t.Context(), []string{"operator", "--state-dir", t.TempDir(), "--config", path, "--center-node", "AAAAAAAAAAAAAAAAAAAAAA", "--bind", "127.0.0.1"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "log in using the operator GUI") {
		t.Fatal(err)
	}
}
