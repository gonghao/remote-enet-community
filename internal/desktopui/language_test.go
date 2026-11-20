package desktopui

import (
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/settings"
)

func TestDisplayLanguageDoesNotModifyControllerOrDiagnostics(t *testing.T) {
	s, _ := testShell(t)
	before := s.controller.Snapshot()
	var report string
	s.copyText = func(value string) { report = value }
	for _, tag := range []string{"zh-Hans", "zh-Hant", "en"} {
		s.language = tag
		s.commandNotice = ""
		s.state.Notice = "Settings saved."
		s.applyState()
		tt := ui.NewTester(s.view.Render, 1080, 1000)
		if !tt.HasText(s.tr("Settings saved.")) {
			t.Fatal("product status was not localized")
		}
		if !s.controller.Snapshot().Config.Equal(before.Config) || s.state.Notice != "Settings saved." {
			t.Fatal("display translation mutated the underlying data")
		}
		s.copyDiagnostics()
		if !strings.Contains(report, `"schemaVersion":1`) || !strings.Contains(report, `"role":"client"`) || strings.Contains(report, "语言") || strings.Contains(report, "語言") {
			t.Fatal("language changed the machine-readable diagnostic format")
		}
	}
}

func TestLocalizedMarkerReceiptAndRawErrorFallback(t *testing.T) {
	s, _ := testShell(t)
	s.language = "zh-Hant"
	if got := s.displayMessage("Marker 42 queued."); got != "標記 42 已加入佇列。" {
		t.Fatalf("marker receipt: %q", got)
	}
	for _, data := range []string{"/tmp/Settings.jsonl", "marker PRIVATE queued.", "Marker PRIVATE queued.", "unexpected protocol error"} {
		if s.displayMessage(data) != data {
			t.Fatal("unknown diagnostic data was interpreted as a message template")
		}
	}
}

func TestBareCodeDisplaysDirectAddressHint(t *testing.T) {
	s, _ := testShell(t)
	for _, center := range []settings.Center{{}, {ID: "garage", API: []string{"https://garage.invalid"}}} {
		if err := s.controller.SetCenter(t.Context(), center); err != nil {
			t.Fatal(err)
		}
		if err := s.controller.StartClient(t.Context(), "12345678"); err == nil {
			t.Fatal("bare code started the client")
		}
		s.state = s.controller.Snapshot()
		for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
			s.language = language
			s.applyState()
			tt := ui.NewTester(s.view.Render, 440, 700)
			if !tt.HasText(s.tr("Enter the direct address and port")) {
				t.Fatalf("missing address hint for %q in %s", center.ID, language)
			}
		}
	}
}
