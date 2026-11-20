package ui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/settings"
)

func TestOperatorModeSummary(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, tc := range []struct {
			mode, share, address, kind, field, action, status string
			waiting                                           bool
		}{
			{"direct", "", "localhost:47000", "Direct", "Public endpoint", "Start listening", "Listening", false},
			{"hub", "public.example:443", "public.example:443", "Self-hosted hub", "Share address", "Connect to hub and wait", "Waiting on hub", true},
			{"hub", "", "hub.example:443", "Self-hosted hub", "Share address", "Connect to hub and wait", "Connecting to hub", false},
		} {
			t.Run(lang+"/"+tc.address, func(t *testing.T) {
				cfg := operatorConfig()
				cfg.Operator.Mode = tc.mode
				cfg.Operator.Hub = settings.Hub{Addr: "hub.example:443", Share: tc.share}
				v := New(cfg, Actions{})
				v.SetLanguage(lang)
				v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseListening, HubWaiting: tc.waiting}})
				for _, size := range []struct {
					w, h int
					zoom float32
				}{{1080, 760, 1}, {900, 620, 1.5}} {
					tt := ui.NewTester(v.Render, size.w, size.h)
					tt.SetZoom(size.zoom)
					for _, text := range []string{v.tr(tc.kind) + " · " + v.tr(tc.field), tc.address, v.tr(tc.action), v.tr(tc.status), v.tr("Numeric code · easy to dictate"), v.tr("Invitation · easy to copy and paste")} {
						scrollTo(t, tt, text, float32(size.w), float32(size.h))
						r, ok := tt.Find(text)
						if !ok || r.W <= 0 || r.H <= 0 || r.X < 0 || r.X+r.W > float32(size.w) {
							t.Fatalf("missing or clipped %q: %+v", text, r)
						}
					}
					if !tt.HasText(v.tr("Client enters %s + an 8-digit code.", tc.address)) {
						t.Fatal("numeric instructions missing the shared address")
					}
				}
			})
		}
	}
}

func TestMissingHubSettingsHint(t *testing.T) {
	cfg := operatorConfig()
	cfg.Operator.Mode = "hub"
	cfg.Operator.Bind = ""
	v := New(cfg, Actions{})
	v.page = sessionPage
	tt := ui.NewTester(v.Render, 1080, 760)
	if !tt.HasText("Complete these settings: Hub address · Hub fingerprint · Presentation IPv4") {
		t.Fatal("missing Hub settings hint")
	}
}

func TestWindowsIdleOperatorBothPanelsFit(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		v := New(operatorConfig(), Actions{})
		v.SetLanguage(lang)
		v.SetState(State{Permission: Permission{Platform: "windows"}})
		tt := ui.NewTester(v.Render, 1080, 760)
		anchor, found := tt.Find(v.tr("Invite Client"))
		if !found || anchor.H == 0 {
			t.Fatal("missing invitation section")
		}
		tt.Scroll(1000, 650, 0, 800)
		if after, _ := tt.Find(v.tr("Invite Client")); after != anchor {
			t.Fatalf("%s scrolled: %+v to %+v", lang, anchor, after)
		}
	}
}

func TestSettingsFooterActions(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
			cfg := operatorConfig()
			cfg.Role = role
			v := New(cfg, Actions{})
			v.SetLanguage(lang)
			v.page = settingsPage
			width, height := 1080, 760
			if role == settings.RoleClient {
				width, height = clientMinWidth, clientMinHeight
			}
			tt := ui.NewTester(v.Render, width, height)
			for _, zoom := range []float32{1, 1.5} {
				tt.SetZoom(zoom)
				scrollTo(t, tt, v.tr("Save settings"), float32(width), float32(height))
				checks, ok := tt.Find(v.tr("Open connection checks"))
				save, found := tt.Find(v.tr("Save settings"))
				if !ok || !found || checks.Y+checks.H/2 != save.Y+save.H/2 || checks.X+checks.W > save.X || save.X+save.W > float32(width) {
					t.Fatalf("%s/%s/%.1f footer overlap: %+v %+v", role, lang, zoom, checks, save)
				}
			}
		}
	}
}

func TestHubNumericDenialStaysInNumericPanel(t *testing.T) {
	cfg := operatorConfig()
	cfg.Operator.Mode = "hub"
	v := New(cfg, Actions{})
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseListening}, NumericCode: pairing.Status{Reason: cpace.Reason(app.HubPairingDisabled)}})
	tt := ui.NewTester(v.Render, 1080, 760)
	const message = "Hub numeric pairing is not enabled for this device"
	if !tt.HasText(message) || !tt.HasText("---- ----") {
		t.Fatal("missing numeric denial or placeholder")
	}
	reason, _ := tt.Find(message)
	editor, ok := tt.Find("Generated invitation")
	if !ok || reason.X+reason.W > editor.X {
		t.Fatal("numeric denial overlaps the invitation panel")
	}
}

func TestInvitationPanelsAlign(t *testing.T) {
	for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
		t.Run(language, func(t *testing.T) {
			v := New(operatorConfig(), Actions{})
			v.SetLanguage(language)
			v.SetState(State{NumericCode: pairing.Status{Reason: cpace.Reason(app.HubPairingDisabled)}})
			buttonWidths := map[string]float32{}
			for _, width := range []int{operatorMinWidth, twoColumnWidth, 1080} {
				tt := ui.NewTester(v.Render, width, 1200)
				bounds := func(role ui.Role, label string) ui.Rect {
					item, ok := tt.AccessByRole(role, v.tr(label))
					if !ok || item.Bounds.W <= 0 || item.Bounds.H <= 0 {
						t.Fatalf("missing %s at %d", label, width)
					}
					return item.Bounds
				}
				numeric := bounds(ui.RoleGroup, "Numeric code · easy to dictate")
				invitation := bounds(ui.RoleGroup, "Invitation · easy to copy and paste")
				if width < twoColumnWidth {
					if numeric.X != invitation.X || numeric.Y+numeric.H > invitation.Y {
						t.Fatalf("panels not stacked: %+v %+v", numeric, invitation)
					}
				} else {
					if numeric.H != invitation.H || numeric.Y != invitation.Y || numeric.X+numeric.W > invitation.X {
						t.Fatalf("unequal or overlapping panels: %+v %+v", numeric, invitation)
					}
					code, found := tt.Find("---- ----")
					output := bounds(ui.RoleTextField, "Generated invitation")
					if !found || code.Y+code.H/2 != output.Y+output.H/2 {
						t.Fatalf("output row misaligned: %+v %+v", code, output)
					}
				}
				expiry, found := tt.Find(v.tr("Expires after 15 minutes"))
				if !found || expiry.X+expiry.W > invitation.X+invitation.W {
					t.Fatalf("expiry outside panel: %+v in %+v", expiry, invitation)
				}
				var buttonY float32
				for i, group := range [][]string{{"Generate numeric code", "Copy address and code", "Revoke code"}, {"Generate invitation", "Copy invitation"}} {
					panel := numeric
					if i == 1 {
						panel = invitation
					}
					end := panel.X
					for j, label := range group {
						r := bounds(ui.RoleButton, label)
						if width == operatorMinWidth {
							buttonWidths[label] = r.W
						}
						if j == 0 && (i == 0 || width < twoColumnWidth) {
							buttonY = r.Y
						}
						if r.X < end || r.X+r.W > panel.X+panel.W || r.Y != buttonY || r.W < buttonWidths[label] {
							t.Fatalf("%s shrank, overflowed or moved off row at %d: %+v in %+v", label, width, r, panel)
						}
						end = r.X + r.W
					}
				}
			}
		})
	}
}
