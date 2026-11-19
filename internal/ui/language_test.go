package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/i18n"
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

func TestLanguageSwitchPreservesEditsAndPendingActions(t *testing.T) {
	var chosen []string
	v := New(operatorConfig(), Actions{})
	state := State{}
	v.actions.SetLanguage = func(tag string) {
		chosen = append(chosen, tag)
		state.ChosenLanguage = tag
		v.SetLanguage(tag)
		v.SetState(state)
	}
	tt := ui.NewTester(v.Render, 1080, 1200)
	openSettings(t, tt)
	input(t, tt, "Public endpoint", "unsaved.example:47000")
	v.directStarting = true
	for _, tag := range []string{"zh-Hans", "zh-Hant", "en"} {
		click(t, tt, v.tr("Interface language"))
		click(t, tt, i18n.Autonym(tag))
		tt.Frame()
		if v.language != tag || !v.dirty() || v.draft.Operator.Endpoint != "unsaved.example:47000" || !v.directStarting {
			t.Fatal("changing language reset the network form or pending action")
		}
		if _, ok := tt.Find(v.tr("Save settings")); !ok {
			t.Fatal("settings controls did not change language")
		}
	}
	if len(chosen) != 3 {
		t.Fatalf("language choice requests: %v", chosen)
	}
}

func TestLocalizedCaptureKeepsOriginalData(t *testing.T) {
	for _, tag := range []string{"zh-Hans", "zh-Hant", "en"} {
		t.Run(tag, func(t *testing.T) {
			var copied string
			v := New(operatorConfig(), Actions{Copy: func(value string) { copied = value }})
			v.SetLanguage(tag)
			v.page = capturePage
			state := State{Running: true, Capture: hsfzcapture.Snapshot{
				Path: "/tmp/Settings.jsonl", StartedAt: time.Unix(1, 0),
				RecentMarkers: []hsfzcapture.Marker{{Sequence: 1, Label: "Settings", At: time.Unix(2, 0)}},
				Recent:        []hsfzcapture.Frame{{Sequence: 1, At: time.Unix(3, 0), Connection: 10, Generation: 5, Direction: hsfzcapture.ToVehicle, Control: 1, BodyLength: 2, PrefixHex: "000000020001f410", Status: "complete"}},
			}}
			v.SetState(state)
			tt := ui.NewTester(v.Render, 1080, 1200)
			click(t, tt, v.tr("Select capture frame %d", 1))
			tt.Frame()
			click(t, tt, v.tr("Copy shown hex"))
			if copied != "000000020001f410" || !tt.HasText(v.tr("Frame status: %s", v.tr("complete"))) {
				t.Fatal("translation changed bytes or missed the frame status")
			}
			if v.state.Capture.RecentMarkers[0].Label != "Settings" || v.state.Capture.Path != state.Capture.Path {
				t.Fatal("annotation or path was translated")
			}
			click(t, tt, v.tr("Capture direction filter"))
			click(t, tt, v.tr(string(hsfzcapture.ToVehicle)))
			tt.Frame()
			if v.captureDirection != string(hsfzcapture.ToVehicle) || !v.captureMatches(state.Capture.Recent[0]) {
				t.Fatal("translated direction corrupted the canonical filter")
			}
		})
	}
}

func TestLocalizedPrimaryControlsRemainVisible(t *testing.T) {
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
			t.Run(tag+"/"+role, func(t *testing.T) {
				cfg := operatorConfig()
				width, height := float32(900), float32(620)
				label := "Stop runtime"
				if role == settings.RoleClient {
					cfg = settings.Default()
					width, height = clientMinWidth, clientMinHeight
				}
				v := New(cfg, Actions{Stop: func() {}, Quit: func() {}, SetLanguage: func(string) {}})
				v.SetLanguage(tag)
				v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseConnected}})
				tt := ui.NewTester(v.Render, int(width), int(height))
				for _, name := range []string{v.tr("Settings"), v.tr("Quit Remote ENET"), v.tr(label)} {
					r, ok := tt.Find(name)
					if !ok || r.X < 0 || r.X+r.W > width+1 || r.Y < 0 || r.Y+r.H > height+1 {
						t.Errorf("control is outside %s: %s %+v", fmt.Sprint(width, "x", height), name, r)
					}
				}
				v.page = settingsPage
				tt.Frame()
				if !tt.HasText(v.tr("Changes apply immediately. Network settings and unsaved edits are kept.")) || strings.Contains(v.tr("Settings"), "%!") {
					t.Fatal("language settings were hidden or malformed")
				}
			})
		}
	}
}

func TestClientTitleBarKeepsLocalizedButtonsClickable(t *testing.T) {
	const width = clientMinWidth
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, zoom := range []float32{1, 1.5} {
			t.Run(fmt.Sprintf("%s/%g", tag, zoom), func(t *testing.T) {
				quits := 0
				v := New(settings.Default(), Actions{Quit: func() { quits++ }})
				v.SetLanguage(tag)
				tt := ui.NewTester(v.Render, width, clientMinHeight)
				tt.SetZoom(zoom)
				for _, page := range []page{sessionPage, settingsPage, logPage, checksPage} {
					v.page = page
					tt.Frame()
					labels := []string{"Settings", "Log", "Quit Remote ENET"}
					if page != sessionPage {
						labels = append([]string{"Back to session"}, labels...)
					}
					var right float32
					for _, label := range labels {
						r, ok := tt.Find(v.tr(label))
						if !ok || r.W <= 0 || r.H <= 0 || r.X < right || r.X+r.W > width || r.Y < 0 || r.Y+r.H > 80 {
							t.Fatalf("page %d button %q clipped or overlapping: %+v", page, label, r)
						}
						right = r.X + r.W
					}
					click(t, tt, v.tr("Quit Remote ENET"))
					if page != sessionPage {
						click(t, tt, v.tr("Back to session"))
						if v.page != sessionPage {
							t.Fatal("Back did not return to the session")
						}
					}
				}
				click(t, tt, v.tr("Settings"))
				if v.page != settingsPage {
					t.Fatal("Settings did not open")
				}
				click(t, tt, v.tr("Log"))
				if v.page != logPage || quits != 4 {
					t.Fatal("Log or Quit did not respond to clicks")
				}
			})
		}
	}
}

func TestClientFormControlsAlignAcrossLanguages(t *testing.T) {
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, zoom := range []float32{1, 1.5} {
			t.Run(fmt.Sprintf("%s/%g", tag, zoom), func(t *testing.T) {
				v := New(settings.Default(), Actions{Save: func(settings.Config) {}})
				v.SetLanguage(tag)
				tt := ui.NewTester(v.Render, clientMinWidth, clientMinHeight)
				tt.SetZoom(zoom)
				adapter, hasAdapter := tt.Access(v.tr("Adapter selection"))
				invitation, hasInvitation := tt.AccessByRole(ui.RoleTextField, v.tr("Invitation code"))
				if !hasAdapter || !hasInvitation || adapter.Bounds.W <= 0 || invitation.Bounds.W <= 0 {
					t.Fatal("adapter selection or invitation field is missing")
				}
				if adapter.Bounds.X != invitation.Bounds.X {
					t.Fatalf("adapter starts at %g, invitation starts at %g", adapter.Bounds.X, invitation.Bounds.X)
				}
			})
		}
	}
}
