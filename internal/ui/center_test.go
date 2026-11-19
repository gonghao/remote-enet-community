package ui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/center"
	"remote-enet/internal/settings"
)

func TestCenterCheckInSettings(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
			t.Run(role+"/"+lang, func(t *testing.T) {
				cfg := operatorConfig()
				cfg.Role = role
				cfg.Center = settings.Center{}
				checks := 0
				v := New(cfg, Actions{CheckCenter: func() { checks++ }})
				v.SetLanguage(lang)
				v.page = settingsPage
				width, height := float32(clientMinWidth), float32(clientMinHeight)
				if role == settings.RoleOperator {
					width, height = operatorMinWidth, operatorMinHeight
				}
				tt := ui.NewTester(v.Render, int(width), int(height))
				tt.SetZoom(1.5)
				button := v.tr("Check center")
				scrollTo(t, tt, button, width, height)
				click(t, tt, button)
				if checks != 0 {
					t.Fatal("unconfigured center check was enabled")
				}

				cfg.Center = settings.Center{ID: "garage", API: []string{
					"https://verified.invalid", "https://unreachable.invalid", "https://mismatch.invalid",
				}}
				v.SetConfig(cfg)
				tt.Frame()
				scrollTo(t, tt, button, width, height)
				click(t, tt, button)
				tt.Frame()
				click(t, tt, button)
				if checks != 1 || !tt.HasText(v.tr("Checking center…")) {
					t.Fatal("check did not report progress or allowed repeated requests")
				}

				results := []center.EntryResult{
					{Entry: cfg.Center.API[0], Status: center.Verified},
					{Entry: cfg.Center.API[1], Status: center.Unreachable},
					{Entry: cfg.Center.API[2], Status: center.IDMismatch},
				}
				v.Complete(ActionCheckCenter)
				v.SetCenterResults(cfg.Center, results)
				tt.Frame()
				for i, label := range []string{"Center verified", "Center unreachable", "Center ID mismatch"} {
					if !tt.HasText(v.tr(label)) || !tt.HasText(cfg.Center.API[i]) {
						t.Errorf("missing result %s for %s", label, cfg.Center.API[i])
					}
				}
				if tt.HasText(v.tr("Checking center…")) {
					t.Fatal("check retained progress after completion")
				}

				oldCenter := cfg.Center
				cfg.Center.ID = "other"
				v.SetConfig(cfg)
				v.SetCenterResults(oldCenter, results)
				tt.Frame()
				if tt.HasText(v.tr("Center verified")) {
					t.Fatal("showed results for a previously saved center")
				}
			})
		}
	}
}
