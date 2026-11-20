package ui

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/settings"
)

func centerConfig() settings.Config {
	c := operatorConfig()
	c.Operator.Mode = "center"
	c.Center = settings.Center{ID: "garage", API: []string{"https://office.invalid"}}
	return c
}

func TestCenterAccountForm(t *testing.T) {
	var code, name string
	v := New(centerConfig(), Actions{CenterLogin: func(c, n string) { code, name = c, n }})
	tt := ui.NewTester(v.Render, 1080, 760)
	for _, tc := range []struct{ input, err string }{{"ABC", "Account code must be 8 characters"}, {"ABC!", "Account code has an invalid character"}} {
		input(t, tt, "Account code", tc.input)
		if !tt.HasText(tc.err) {
			t.Fatalf("missing %q", tc.err)
		}
		click(t, tt, "Sign in")
		if code != "" {
			t.Fatal("submitted invalid code")
		}
	}
	input(t, tt, "Account code", "abcd-0123")
	click(t, tt, "Sign in")
	if code != "ABCD0123" || name != "" {
		t.Fatalf("unexpected login: %q %q", code, name)
	}
}

func TestCenterNodePreference(t *testing.T) {
	nodes := []centerflow.NodeLatency{}
	for i, d := range []time.Duration{100, 10, -1, 1} {
		n := center.Node{NodeID: center.NodeID{byte(i + 1)}, State: "available"}
		if i == 3 {
			n.State = "full"
		}
		nodes = append(nodes, centerflow.NodeLatency{Node: n, Latency: d})
	}
	for _, tc := range []struct {
		selected string
		want     int
	}{{"", 1}, {nodes[0].NodeID.String(), 0}, {nodes[3].NodeID.String(), 1}} {
		if preferredNode(nodes, tc.selected) != nodes[tc.want].NodeID.String() {
			t.Fatal("wrong node")
		}
	}
}

func TestCenterSASDialog(t *testing.T) {
	for _, role := range []string{settings.RoleOperator, settings.RoleClient} {
		t.Run(role, func(t *testing.T) {
			cfg := centerConfig()
			cfg.Role = role
			prompt := centerflow.SASPrompt{ID: 1, Code: "001234", Deadline: time.Now().Add(time.Minute)}
			var answers []bool
			v := New(cfg, Actions{CenterDisplay: func() (string, centerflow.SASPrompt) { return "", prompt }, MatchSAS: func(id uint64, input string) bool {
				digits, ok := sasDigits(input)
				return ok && id == prompt.ID && digits == "001234"
			}, AnswerSAS: func(id uint64, match bool) {
				if id != prompt.ID {
					t.Fatal("stale prompt")
				}
				answers = append(answers, match)
			}})
			s := State{}
			s.Center.SASID, s.Center.SASDeadline = prompt.ID, prompt.Deadline
			v.SetState(s)
			tt := ui.NewTester(v.Render, 440, 700)
			if !tt.HasText("001 234") {
				t.Fatal("missing local SAS")
			}
			label := "Confirm"
			if role == settings.RoleOperator {
				for _, val := range []string{"", "123", "123456"} {
					if val != "" {
						input(t, tt, "Short code", val)
					}
					if info, ok := tt.AccessByRole(ui.RoleButton, label); !ok || !info.Disabled {
						t.Fatal("confirm should be disabled")
					}
					click(t, tt, label)
					if len(answers) != 0 {
						t.Fatal("accepted unmatched input")
					}
				}
				if !tt.HasText("Does not match this screen") {
					t.Fatal("missing mismatch hint")
				}
				input(t, tt, "Short code", "００１ ２３４")
			} else {
				label = "Confirm (read to operator)"
			}
			click(t, tt, label)
			if len(answers) != 1 || !answers[0] {
				t.Fatalf("could not confirm, input %q", v.sasInput)
			}
			prompt.ID++
			s.Center.SASID++
			v.SetState(s)
			tt.Frame()
			click(t, tt, "Mismatch")
			if len(answers) != 2 || answers[1] {
				t.Fatal("could not reject new prompt")
			}
			s.Center.SASID = 0
			v.SetState(s)
			tt.Frame()
			if tt.HasText("Verify the short code") {
				t.Fatal("stale dialog")
			}
		})
	}
}

func TestOperatorModesLayout(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, mode := range []string{"direct", "hub", "center"} {
			for _, p := range []page{sessionPage, settingsPage} {
				for _, scale := range []float32{1, 1.5} {
					t.Run(fmt.Sprintf("%s/%s/%d/%.1f", lang, mode, p, scale), func(t *testing.T) {
						cfg := centerConfig()
						cfg.Operator.Mode = mode
						v := New(cfg, Actions{})
						v.SetLanguage(lang)
						v.page = p
						width, height := 1080, 760
						if scale > 1 {
							width, height = 900, 620
						}
						tt := ui.NewTester(v.Render, width, height)
						tt.SetScale(scale)
						tt.SetZoom(scale)
						var key string
						if p == sessionPage {
							key = "Invite Client"
							if tt.HasText(v.tr("Invitation · easy to copy and paste")) != (mode != "center") {
								t.Fatal("wrong invitation panels")
							}
						} else {
							key = map[string]string{"direct": "Public endpoint", "hub": "Hub address", "center": "Center ID"}[mode]
						}
						if _, ok := tt.Find(v.tr(key)); !ok {
							t.Fatalf("missing %s", key)
						}
						if scale == 1 {
							before, _ := tt.Find(v.tr(key))
							tt.Scroll(530, 380, 0, 900)
							if after, _ := tt.Find(v.tr(key)); after != before {
								t.Errorf("default page scrolls by %.0f DIP", before.Y-after.Y)
							}
						}
					})
				}
			}
		}
	}
}

func TestClientCenterRetry(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		v := New(settings.Default(), Actions{RetryPairing: func() {}})
		v.SetLanguage(lang)
		s := State{Running: true}
		s.Center.Phase = "retry"
		s.Center.Reason = centerflow.Confirmation
		s.Center.AttemptsLeft = 2
		v.SetState(s)
		tt := ui.NewTester(v.Render, 440, 700)
		if !tt.HasText(v.centerReason()) {
			t.Fatal("missing retry reason and attempts")
		}
		button, ok := tt.AccessByRole(ui.RoleButton, v.tr("Retry pairing"))
		if !ok || button.Disabled {
			t.Fatal("retry button unavailable")
		}
		tt.ClickAt(button.Bounds.X+button.Bounds.W/2, button.Bounds.Y+button.Bounds.H/2)
		if !v.retryPending {
			t.Fatal("retry action unavailable")
		}
		tt.SetSize(420, 480)
		tt.SetZoom(1.5)
		scrollTo(t, tt, v.tr("Retry pairing"), 420, 480)
	}
}

// The smallest viewport that keeps the scroll origin fixed includes the header
// and bottom padding, independently of which localized card is tallest.
func TestCenterContentHeight(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, role := range []string{settings.RoleOperator, settings.RoleClient} {
			for _, p := range []page{sessionPage, settingsPage} {
				cfg := centerConfig()
				cfg.Role = role
				v := New(cfg, Actions{})
				v.page = p
				v.SetLanguage(lang)
				width, limit := 1080, 760
				anchor := "Invite Client"
				if role == settings.RoleClient {
					width, limit = 440, 700
					anchor = "Adapter selection"
				}
				if p == settingsPage {
					anchor = "Save settings"
				}
				fits := func(height int) bool {
					tt := ui.NewTester(v.Render, width, height)
					before, ok := tt.Find(v.tr(anchor))
					if !ok {
						t.Fatal("missing scroll anchor")
					}
					tt.Scroll(float32(width/2), float32(height/2), 0, 900)
					after, _ := tt.Find(v.tr(anchor))
					return before == after
				}
				if !fits(limit) {
					t.Fatalf("%s %s page %d scrolls at default height", lang, role, p)
				}
				lo, hi := 60, limit
				for lo < hi {
					mid := (lo + hi) / 2
					if fits(mid) {
						hi = mid
					} else {
						lo = mid + 1
					}
				}
				t.Logf("%s %s page %d needs %d DIP", lang, role, p, lo)
			}
		}
	}
}

func TestCenterReceptionActions(t *testing.T) {
	var selected, copied string
	refreshes, ends := 0, 0
	v := New(centerConfig(), Actions{StartReception: func(id string) { selected = id }, RefreshCenter: func() { refreshes++ }, EndReception: func() { ends++ }, Copy: func(code string) { copied = code }, CenterDisplay: func() (string, centerflow.SASPrompt) { return "00123456", centerflow.SASPrompt{} }})
	s := State{}
	s.Center.LoggedIn = true
	s.Center.Nodes = []centerflow.NodeLatency{{Node: center.Node{Region: "west", NodeID: center.NodeID{1}, State: "available"}, Latency: 3 * time.Millisecond}}
	v.SetState(s)
	tt := ui.NewTester(v.Render, 1080, 760)
	if refreshes != 1 {
		t.Fatal("missing entry refresh")
	}
	v.Complete(ActionRefreshCenter)
	tt.Frame()
	click(t, tt, "Start reception")
	if selected != s.Center.Nodes[0].NodeID.String() {
		t.Fatal("wrong reception node")
	}
	s.Running = true
	s.Center.Phase = "open"
	s.Center.CodeExpiresAt = time.Now().Add(time.Minute)
	s.Center.AttemptsLeft = 3
	v.SetState(s)
	tt.Frame()
	if !tt.HasText("0012 3456") {
		t.Fatal("missing grouped code")
	}
	click(t, tt, "Copy numeric code")
	if copied != "00123456" {
		t.Fatal("unexpected clipboard contents")
	}
	s.Center.Phase = "redeemed"
	s.Center.PeerShort = "abcdef"
	v.SetState(s)
	tt.Frame()
	if tt.HasText("0012 3456") || !tt.HasText("Redeemed · visitor abcdef") {
		t.Fatal("code not replaced after redemption")
	}
	click(t, tt, "End reception")
	if ends != 1 {
		t.Fatal("end not sent")
	}
}

func TestClientSessionRecovery(t *testing.T) {
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		t.Run(lang, func(t *testing.T) {
			cfg := centerConfig()
			cfg.Role = settings.RoleClient
			now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.Local)
			older := centerflow.SessionCandidate{ID: center.SessionID{1}, ShortID: "010000", RedeemedAt: now}
			newer := centerflow.SessionCandidate{ID: center.SessionID{2}, ShortID: "020000", RedeemedAt: now.Add(time.Minute)}
			items := []centerflow.SessionCandidate{older, newer}
			var selected center.SessionID
			v := New(cfg, Actions{
				CenterSessionCandidates: func() []centerflow.SessionCandidate { return slices.Clone(items) },
				ResumeCenterSession:     func(id center.SessionID) { selected = id },
			})
			v.SetLanguage(lang)
			tt := ui.NewTester(v.Render, 440, 700)
			title := v.tr("Choose a session to recover")
			first := "020000 · 2026-11-20 08:01:00"
			second := "010000 · 2026-11-20 08:00:00"
			a, okA := tt.Find(first)
			b, okB := tt.Find(second)
			if !tt.HasText(title) || !okA || !okB || a.Y >= b.Y || tt.HasText(older.ID.String()) || tt.HasText(newer.ID.String()) {
				t.Fatal("expected only short IDs and redemption times, newest first")
			}
			tt.Scroll(220, 590, 0, 600)
			if after, _ := tt.Find(second); after != b {
				t.Fatal("default recovery page scrolled")
			}
			click(t, tt, second)
			if selected != older.ID {
				t.Fatal("wrong recovery selection")
			}
			v.actions.ResumeCenterSession = nil
			tt.Frame()
			if action, ok := tt.AccessByRole(ui.RoleButton, first); !ok || !action.Disabled {
				t.Fatal("recovery action should require a controller handler")
			}
			items = nil
			tt.Frame()
			if tt.HasText(title) || tt.HasText(first) {
				t.Fatal("stale recovery list")
			}
		})
	}
}
