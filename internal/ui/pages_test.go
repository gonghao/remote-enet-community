package ui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/identity"
	"remote-enet/internal/settings"
)

func TestFixedRoleAcrossPagesAndConfigUpdates(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			cfg := operatorConfig()
			cfg.Role = role
			var saved []settings.Config
			v := New(cfg, Actions{Save: func(c settings.Config) { saved = append(saved, c) }})
			v.SetState(State{Events: []string{"Peer connected."}})
			tt := ui.NewTester(v.Render, 1080, 1000)
			assertRoleControls(t, tt, role)
			wrong := cfg
			if role == settings.RoleClient {
				wrong.Role = settings.RoleOperator
			} else {
				wrong.Role = settings.RoleClient
			}
			wrong.AllowLinkTest = !cfg.AllowLinkTest
			v.SetConfig(wrong)
			tt.Frame()
			if v.saved != cfg || v.draft != cfg {
				t.Fatal("a mismatched config replaced the fixed role or saved settings")
			}
			openSettings(t, tt)
			assertRoleControls(t, tt, role)
			for _, label := range []string{"Adapter selection", "Refresh adapters"} {
				if _, found := tt.Find(label); found {
					t.Errorf("%s Settings exposed %q", role, label)
				}
			}
			click(t, tt, "Allow peer connection tests")
			click(t, tt, "Save settings")
			if len(saved) != 1 || saved[0].Role != role || !v.dirty() {
				t.Fatal("Settings did not submit changes with the fixed application role")
			}
			v.SetConfig(wrong)
			if v.submitted == nil || v.draft.Role != role {
				t.Fatal("a mismatched config acknowledged the pending save")
			}
			v.SetConfig(saved[0])
			v.SetState(State{Events: []string{"Peer connected."}})
			v.Complete(ActionSave)
			tt.Frame()
			openSession(t, tt)
			assertRoleControls(t, tt, role)
			click(t, tt, "Log")
			if !tt.HasText("Peer connected.") {
				t.Fatal("Log did not show current runtime events")
			}
			assertRoleControls(t, tt, role)
			openSession(t, tt)
			if v.role != role || v.draft.Role != role || v.dirty() {
				t.Fatal("page navigation changed the fixed role or saved form")
			}
		})
	}
}

func assertRoleControls(t *testing.T, tt *ui.Tester, role string) {
	t.Helper()
	absent := []string{"Workspace role"}
	if role == settings.RoleClient {
		absent = append(absent, "Start operator", "Generate invitation", "Copy invitation", "Generated invitation", "Public endpoint", "Exclusive diagnostic tool access")
	} else {
		absent = append(absent, "Connect client", "Invite or address:port and code", "Adapter selection", "Refresh adapters")
	}
	for _, label := range absent {
		if _, found := tt.Find(label); found {
			t.Errorf("%s page exposed %q", role, label)
		}
	}
}

func TestDashboardPrimaryControlsAreVisible(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		for _, connected := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/connected=%t", role, connected), func(t *testing.T) {
				cfg := operatorConfig()
				cfg.Role = role
				v := New(cfg, Actions{})
				state := State{}
				if connected {
					state = previewState()
				}
				v.SetState(state)
				width, height := clientWidth, clientHeight
				labels := []string{"Connect client", "Stop runtime", "Invite or address:port and code", "Adapter selection", "Refresh adapters", "Settings", "Log"}
				if role == settings.RoleOperator {
					width, height = 1080, 760
					labels = []string{"Start operator", "Stop runtime", "Generate invitation", "Copy invitation", "Test connection", "Settings", "Log"}
				}
				tt := ui.NewTester(v.Render, width, height)
				for _, label := range labels {
					r, found := tt.Find(label)
					if !found || r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > float32(width) || r.Y+r.H > float32(height) {
						t.Errorf("primary control %q is not visible: %+v", label, r)
					}
				}
				for _, absent := range []string{"Public endpoint", "Save settings"} {
					if _, found := tt.Find(absent); found {
						t.Errorf("dashboard still contains configuration control %q", absent)
					}
				}
				if !tt.HasText("VEHICLE") || !tt.HasText("CONNECTION") || !tt.HasText("SESSION") {
					t.Fatal("dashboard did not expose all three information groups")
				}
			})
		}
	}
}

func TestClientDefaultWindowFitsHomeWithoutScrolling(t *testing.T) {
	for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, running := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/connected=%t", language, running), func(t *testing.T) {
				v := New(settings.Default(), Actions{})
				v.SetLanguage(language)
				state := State{}
				if running {
					state = clientPreviewState()
				}
				state.Notice = "Interfaces refreshed."
				v.SetState(state)
				// Shrink the viewport to verify that the full content, including
				// its bottom padding, leaves room for taller platform fonts.
				const height = clientHeight - 24
				tt := ui.NewTester(v.Render, clientWidth, height)
				if !tt.HasText(v.tr(state.Notice)) {
					t.Fatal("adapter refresh notice is missing")
				}
				last := v.tr("Next retry")
				bottom, ok := tt.Find(last)
				if !ok || bottom.Y < 0 || bottom.H <= 0 || bottom.Y+bottom.H > height {
					t.Fatalf("last information row encroaches on the 24 DIP reserve: %+v", bottom)
				}
				tt.Scroll(clientWidth/2, height/2, 0, 600)
				if after, _ := tt.Find(last); after != bottom {
					t.Fatalf("default dashboard scrolled: %+v → %+v", bottom, after)
				}
			})
		}
	}
}

func TestLogIsBoundedAndDoesNotExposeInvitation(t *testing.T) {
	v := New(operatorConfig(), Actions{})
	state := State{Invitation: "renet1:TEST_ONLY_NOT_A_CREDENTIAL"}
	for i := range 250 {
		state.Events = append(state.Events, fmt.Sprintf("Event %03d", i))
	}
	v.SetState(state)
	tt := ui.NewTester(v.Render, 1080, 760)
	click(t, tt, "Log")
	if tt.HasText(state.Invitation) || !tt.HasText("Event 249") || tt.HasText("Event 049") {
		t.Fatal("Log leaked an invitation or rendered out-of-bound history")
	}
	count := 0
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "Event ") {
			count++
		}
	}
	if count != 200 {
		t.Fatalf("Log rendered %d events, want 200", count)
	}
	tt.Scroll(1068, 380, 0, 12000)
	scrollTo(t, tt, "Event 050", 1080, 760)
	click(t, tt, "Back to session")
	if _, ok := tt.Find("Start operator"); !ok {
		t.Fatal("Log could not return to the single-session dashboard")
	}
}

func TestClientSettingsPreserveInvitationAndEdits(t *testing.T) {
	var saved settings.Config
	cfg := settings.Default()
	cfg.Client.Peer = (identity.Fingerprint{1}).String()
	v := New(cfg, Actions{Save: func(c settings.Config) { saved = c }})
	tt := ui.NewTester(v.Render, clientWidth, clientHeight)
	input(t, tt, "Invite or address:port and code", "test-only-paste")
	openSettings(t, tt)
	if !tt.HasText("Remembered peer") || !tt.HasText((identity.Fingerprint{1}).Short()) {
		t.Fatal("Settings did not show the remembered peer")
	}
	click(t, tt, "Allow peer connection tests")
	click(t, tt, "Log")
	v.SetState(State{Runtime: app.Snapshot{Phase: app.PhaseIdle}, Notice: "Adapters refreshed."})
	tt.Frame()
	openSettings(t, tt)
	click(t, tt, "Save settings")
	if saved.AllowLinkTest || strings.Contains(fmt.Sprint(saved), "test-only-paste") || v.invitation != "test-only-paste" {
		t.Fatal("navigation or state update lost edits or persisted an invitation")
	}
}
