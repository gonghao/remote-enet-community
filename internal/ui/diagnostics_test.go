package ui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/settings"
)

func TestDiagnosticCopyRequiresExplicitLogAction(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			cfg := operatorConfig()
			cfg.Role = role
			copies := 0
			v := New(cfg, Actions{CopyDiagnostics: func() { copies++ }})
			tt := ui.NewTester(v.Render, 1080, 760)
			v.SetState(State{Invitation: "private invitation", Error: "an earlier error"})
			tt.Frame()
			if copies != 0 {
				t.Fatal("a snapshot copied diagnostics without user input")
			}
			click(t, tt, "Log")
			click(t, tt, "Copy diagnostic report")
			if copies != 1 {
				t.Fatal("explicit diagnostic copy was not delivered")
			}
			v.SetState(State{})
			tt.Frame()
			if copies != 1 {
				t.Fatal("subsequent snapshots copied diagnostics again")
			}
		})
	}
}

func TestNetworkPermissionHintsKeepDirectControlsVisible(t *testing.T) {
	for _, p := range []Permission{
		{Platform: "darwin", HelperAvailable: true, AuthorizationRequired: true},
		{Platform: "darwin"},
		{Platform: "windows"},
		{Platform: "windows", Elevated: true},
	} {
		t.Run(p.Platform+vPermissionKey(p), func(t *testing.T) {
			v := New(operatorConfig(), Actions{ConnectMGU: func() {}, DisconnectMGU: func() {}})
			s := previewState()
			s.Notice, s.Permission = "Peer connected.", p
			v.SetState(s)
			tt := ui.NewTester(v.Render, 1080, 720)
			if !tt.HasText(v.permissionHint()) {
				t.Fatal("permission guidance was not displayed")
			}
			for _, label := range []string{"Enable MGU direct link", "Disable head unit direct link"} {
				scrollTo(t, tt, label, 1080, 720)
				r, found := tt.Find(label)
				if !found || r.Y < 0 || r.Y+r.H > 720 {
					t.Fatalf("permission guidance hid %s: %+v", label, r)
				}
			}
		})
	}
}

func vPermissionKey(p Permission) string {
	if p.Elevated {
		return "/elevated"
	}
	if p.HelperAvailable {
		return "/authorization"
	}
	return "/unavailable"
}
