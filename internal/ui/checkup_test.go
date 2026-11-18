package ui

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/checkup"
	"remote-enet/internal/locales"
	"remote-enet/internal/settings"
)

func TestChecksRequestLatchesAndPendingCancellation(t *testing.T) {
	starts, cancels := 0, 0
	v := New(settings.Default(), Actions{
		CheckConnection: func(code string) {
			starts++
			if code != "renet1:in-memory" {
				t.Error("current invitation not used")
			}
		},
		CancelChecks: func() { cancels++ },
	})
	v.invitation = " renet1:in-memory "
	tt := ui.NewTester(v.Render, 900, 800)
	click(t, tt, "Log")
	click(t, tt, "Open connection checks")
	click(t, tt, "Run connection checks")
	for _, action := range []Action{ActionRefresh, ActionSave, ActionCancelChecks} {
		v.SetState(State{Error: "Old error"})
		v.Complete(action)
		tt.Frame()
		click(t, tt, "Run connection checks")
	}
	if starts != 1 {
		t.Fatal("old state or unrelated completion unlocked checks")
	}
	click(t, tt, "Cancel connection checks")
	if cancels != 1 {
		t.Fatal("pending start could not be canceled")
	}
	v.Complete(ActionCheckConnection)
	v.SetState(State{Checking: true})
	tt.Frame()
	click(t, tt, "Cancel connection checks")
	if cancels != 1 {
		t.Fatal("run completion unlocked cancellation")
	}
	v.Complete(ActionCancelChecks)
	tt.Frame()
	click(t, tt, "Cancel connection checks")
	if cancels != 2 {
		t.Fatal("matching completion did not release cancellation")
	}
	v.SetState(State{})
	v.Complete(ActionCancelChecks)
	tt.Frame()
	click(t, tt, "Run connection checks")
	if starts != 2 {
		t.Fatal("checks could not be repeated")
	}
}

func TestChecksLocalizedResultsAndPrivateCopy(t *testing.T) {
	for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
			t.Run(language+"/"+role, func(t *testing.T) {
				cfg := operatorConfig()
				cfg.Role = role
				copied := ""
				v := New(cfg, Actions{Copy: func(value string) { copied = value }, CheckConnection: func(string) {}})
				v.SetLanguage(language)
				v.page = checksPage
				now := time.Now()
				report := checkup.Report{StartedAt: now, FinishedAt: now, Complete: true, Findings: []checkup.Finding{
					{ID: "hub_tcp", Index: 1, Status: checkup.Pass, Code: "tcp_reachable"},
					{ID: "discovery_port", Status: checkup.Skipped, Code: "runtime_active"},
					{ID: "SECRET", Status: "SECRET", Code: "renet1:SECRET"},
				}}
				v.SetState(State{Running: true, Checkup: report})
				report.Findings[0].Code = "mutated"
				tt := ui.NewTester(v.Render, 900, 1300)
				if !tt.HasText(v.tr(checkHint("tcp_reachable"))) || !tt.HasText(v.tr(checkHint("runtime_active"))) {
					t.Fatal("result/hint missing")
				}
				for _, text := range tt.Texts() {
					if strings.Contains(text, "SECRET") {
						t.Fatal("raw result reached the UI")
					}
				}
				if copied != "" {
					t.Fatal("report copied without a click")
				}
				click(t, tt, v.tr("Copy connection check report"))
				var result map[string]any
				if err := json.Unmarshal([]byte(copied), &result); err != nil || result["schema"] != "checkup/v1" || strings.Contains(copied, "SECRET") {
					t.Fatalf("copy: %s %v", copied, err)
				}
				v.page = settingsPage
				tt.Frame()
				scrollTo(t, tt, v.tr("Open connection checks"), 900, 1300)
				click(t, tt, v.tr("Open connection checks"))
				if v.page != checksPage {
					t.Fatal("settings has no self-check entry")
				}
			})
		}
	}
}

func TestChecksDirtySettingsAndSmallWindow(t *testing.T) {
	calls := 0
	v := New(operatorConfig(), Actions{CheckConnection: func(string) { calls++ }, CancelChecks: func() {}})
	v.page = checksPage
	v.draft.Operator.Endpoint = "unsaved.invalid:47000"
	tt := ui.NewTester(v.Render, 420, 420)
	click(t, tt, "Run connection checks")
	if calls != 0 || !tt.HasText("Save changes in Settings before running checks.") {
		t.Fatal("unsaved settings were checked")
	}
	v.resetDraft(v.saved)
	v.SetState(State{Checking: true})
	tt.Frame()
	for _, label := range []string{"Run connection checks", "Cancel connection checks", "Back to session"} {
		r, ok := tt.Find(label)
		if !ok || r.X < 0 || r.Y < 0 || r.X+r.W > 421 || r.Y+r.H > 421 {
			t.Fatalf("small-window action hidden: %s %+v", label, r)
		}
	}
}

func TestCheckFindingVocabularyIsLocalized(t *testing.T) {
	for _, tag := range []string{"zh-Hans", "zh-Hant"} {
		for _, source := range []string{checkHint("target_conflict"), checkTitle("mgu_permission"), checkStatus(checkup.Skipped), "Connection checks canceled: connection settings changed."} {
			if locales.Text(tag, source) == source {
				t.Errorf("untranslated %s: %s", tag, source)
			}
		}
	}
}
