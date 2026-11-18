package desktopui

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiagnosticCopyUsesFreshSnapshotAndDoesNotCopySecrets(t *testing.T) {
	s, _ := testShell(t)
	s.version = "0.1.0-preview.wave10.0-worktree"
	s.state.Invitation = "PRIVATE_INVITATION"
	s.state.Runtime.LastError = "PRIVATE_ERROR"
	s.state.Runtime.VIN = "PRIVATE_VEHICLE"
	s.state.Runtime.Generation = 999
	var copied []string
	s.copyText = func(value string) { copied = append(copied, value) }
	s.copyDiagnostics()
	if len(copied) != 1 || !json.Valid([]byte(copied[0])) {
		t.Fatal("explicit copy did not produce one JSON report")
	}
	for _, forbidden := range []string{"PRIVATE", "999", "invitation", "VIN", "ConfigPath"} {
		if strings.Contains(copied[0], forbidden) {
			t.Fatalf("diagnostic report contained stale or excluded data: %q", forbidden)
		}
	}
	if !strings.Contains(copied[0], s.version) || s.state.Notice != "Diagnostic report copied." {
		t.Fatal("copy did not preserve the build version or acknowledge success")
	}
	// Metrics subscriptions must not erase the explicit copy receipt immediately.
	s.state = s.controller.Snapshot()
	s.applyState()
	if s.commandNotice != "Diagnostic report copied." {
		t.Fatal("subscription erased diagnostic copy feedback")
	}
	s.quitting = true
	s.copyDiagnostics()
	if len(copied) != 1 {
		t.Fatal("quitting shell copied another report")
	}
}

func TestDiagnosticFailureKeepsClipboardAndErrorPrivate(t *testing.T) {
	s, _ := testShell(t)
	s.version = "PRIVATE_INVALID_VERSION"
	s.copyText = func(string) { t.Fatal("invalid report changed clipboard") }
	s.copyDiagnostics()
	if s.commandErr != "Diagnostic report could not be copied." || strings.Contains(s.state.Error, "PRIVATE") {
		t.Fatal("diagnostic failure disclosed its input")
	}
}
