package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/settings"
)

func TestNumericOperatorControlsAtMinimumSize(t *testing.T) {
	for _, zoom := range []float32{1, 1.5} {
		for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
			t.Run(fmt.Sprintf("%s/%.1f", language, zoom), func(t *testing.T) {
				issued := 0
				var copied, revoked uint64
				v := New(operatorConfig(), Actions{
					IssueNumericCode:  func() { issued++ },
					CopyNumericCode:   func(generation uint64) { copied = generation },
					RevokeNumericCode: func(generation uint64) { revoked = generation },
				})
				v.SetLanguage(language)
				status := pairing.Status{Address: "operator.example:47000"}
				state := State{Runtime: app.Snapshot{Phase: app.PhaseStarting}, Running: true, NumericCode: status}
				v.SetState(state)
				tt := ui.NewTester(v.Render, 900, 620)
				tt.SetZoom(zoom)
				issue := v.tr("Generate numeric code")
				copyCode := v.tr("Copy address and code")
				revoke := v.tr("Revoke code")
				for _, label := range []string{issue, copyCode, revoke} {
					scrollTo(t, tt, label, 900, 620)
					r, _ := tt.Find(label)
					if r.X < 0 || r.W <= 0 || r.X+r.W > 900 {
						t.Fatalf("%q out of bounds: %+v", label, r)
					}
					click(t, tt, label)
				}
				if issued != 0 || copied != 0 || revoked != 0 {
					t.Fatal("startup admitted a pairing operation")
				}
				state.Runtime.Phase = app.PhaseListening
				v.SetState(state)
				tt.Frame()
				click(t, tt, issue)
				click(t, tt, issue)
				if issued != 1 {
					t.Fatal("listening did not admit exactly one pending generation request")
				}
				status.Generation, status.AttemptsLeft = 1, 4
				status.ExpiresAt = time.Now().Add(72 * time.Second)
				state.NumericCode = status
				v.SetState(state)
				v.SetNumericCodeDisplay(pairing.Display{Status: status, Code: "00001234"})
				v.Complete(ActionIssueNumericCode)
				tt.Frame()
				scrollTo(t, tt, "0000 1234", 900, 620)
				expires := v.tr("Expires in %s", "TIMER")
				expires = strings.Split(expires, "TIMER")[0]
				if !tt.HasText(expires) || !tt.HasText(v.tr("Attempts left: %d", 4)) {
					t.Fatal("the live code lacks a countdown or remaining attempt count")
				}
				click(t, tt, copyCode)
				v.Complete(ActionCopyNumericCode)
				tt.Frame()
				click(t, tt, revoke)
				v.Complete(ActionRevokeNumericCode)
				if copied != 1 || revoked != 1 {
					t.Fatal("live actions did not carry the selected generation")
				}
				state.NumericCode.Committing = true
				v.SetState(state)
				tt.Frame()
				click(t, tt, issue)
				if !tt.HasText(v.tr("Completing pairing")) || tt.HasText("0000 1234") || issued != 1 {
					t.Fatal("committing retained the code or allowed regeneration")
				}
				state.NumericCode = status
				state.NumericCode.Generation++
				v.SetState(state)
				v.SetNumericCodeDisplay(pairing.Display{Status: status, Code: "00001234"})
				tt.Frame()
				if tt.HasText("0000 1234") {
					t.Fatal("delayed display repopulated the secret for an old generation")
				}
				v.SetNumericCodeDisplay(pairing.Display{Status: state.NumericCode, Code: "00001234"})
				state.NumericCode.ExpiresAt = time.Now().Add(-time.Second)
				state.NumericCode.Reason = cpace.ReasonExpired
				v.SetState(state)
				tt.Frame()
				if !tt.HasText(v.tr("Code expired")) || tt.HasText("0000 1234") || v.numericDisplay.Code != "" {
					t.Fatal("expiration did not clear the UI secret")
				}
			})
		}
	}
}

func TestNumericClientInputClearedAtMinimumSize(t *testing.T) {
	for _, zoom := range []float32{1, 1.5} {
		t.Run(fmt.Sprint(zoom), func(t *testing.T) {
			const inputLabel = "Invite or address:port and code"
			const pairingInput = "[2001:db8::2]:47000 ００００ １２３４"
			submitted := ""
			v := New(settings.Default(), Actions{Connect: func(input string) { submitted = input }})
			tt := ui.NewTester(v.Render, clientMinWidth, clientMinHeight)
			tt.SetZoom(zoom)
			scrollTo(t, tt, inputLabel, clientMinWidth, clientMinHeight)
			click(t, tt, inputLabel)
			tt.SetClipboard(pairingInput)
			tt.Command("paste")
			scrollTo(t, tt, "Connect client", clientMinWidth, clientMinHeight)
			click(t, tt, "Connect client")
			if submitted != pairingInput || v.invitation != "" {
				t.Fatal("numeric input was not submitted and cleared immediately")
			}
			v.Complete(ActionConnect)
			tt.Frame()
			scrollTo(t, tt, inputLabel, clientMinWidth, clientMinHeight)
			click(t, tt, inputLabel)
			tt.Command("undo")
			if v.invitation != "" || tt.HasText("００００ １２３４") {
				t.Fatal("editor undo restored submitted pairing material")
			}
		})
	}
}
