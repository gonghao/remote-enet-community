package ui

import (
	"fmt"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
)

// NumericCodeCurrent checks a deferred display or clipboard result against the view.
func (v *View) NumericCodeCurrent(generation uint64) bool {
	status := v.state.NumericCode
	return v.state.Running && !v.state.Stopping && !v.stopping && generation != 0 &&
		generation == status.Generation && !status.Committing && !status.Redeemed && status.Reason == "" && time.Now().Before(status.ExpiresAt)
}

// SetNumericCodeDisplay accepts the result of an explicit generation request.
func (v *View) SetNumericCodeDisplay(display pairing.Display) {
	if len(display.Code) == 8 && v.NumericCodeCurrent(display.Generation) {
		v.numericDisplay = display
	}
}

// ClearPairingSecrets also discards editor state when the window closes.
func (v *View) ClearPairingSecrets() {
	v.clearPairingInput()
	v.numericDisplay = pairing.Display{}
	v.accountCode, v.sasInput = "", ""
	v.sasMatched = false
}

func (v *View) clearExpiredNumericCode() {
	if !v.NumericCodeCurrent(v.numericDisplay.Generation) {
		v.numericDisplay = pairing.Display{}
	}
}

func (v *View) numericCodeControls(c *ui.Context) {
	status := v.state.NumericCode
	current := v.NumericCodeCurrent(status.Generation)
	idle := v.numericAction == 0
	canIssue := idle && v.state.Running && !v.state.Stopping && !v.stopping &&
		v.state.Runtime.Phase == app.PhaseListening && status.Address != "" && !status.Committing

	ui.Row(c.Key("numeric-code-status")).Height(pairingOutputHeight).AlignItems(ui.Center).Gap(12).Children(func() {
		code, color := "---- ----", muted
		if secret := v.numericDisplay.Code; secret != "" {
			code, color = secret[:4]+" "+secret[4:], c.Theme().Text
		}
		ui.Text(c, code).Font("monospace").FontSize(16).TextColor(color).Shrink(0)
		if current {
			seconds := int(max(0, time.Until(status.ExpiresAt).Round(time.Second)) / time.Second)
			ui.Text(c, v.tr("Expires in %s", fmt.Sprintf("%d:%02d", seconds/60, seconds%60))).Font("monospace").FontSize(11).TextColor(muted)
			ui.Text(c, v.tr("Attempts left: %d", status.AttemptsLeft)).Font("monospace").FontSize(11).TextColor(muted)
		}
	})
	ui.Row(c.Key("numeric-code-controls")).Gap(8).Children(func() {
		if ui.Button(c.Key("issue-numeric-code"), v.tr("Generate numeric code")).Padding(4, 8).Height(32).Disabled(!canIssue || v.actions.IssueNumericCode == nil).Clicked() {
			v.numericAction = ActionIssueNumericCode
			v.actions.IssueNumericCode()
		}
		if ui.Button(c.Key("copy-numeric-code"), v.tr("Copy address and code")).Padding(4, 8).Height(32).Disabled(!idle || !current || v.actions.CopyNumericCode == nil).Clicked() {
			v.numericAction = ActionCopyNumericCode
			v.actions.CopyNumericCode(status.Generation)
		}
		if ui.Button(c.Key("revoke-numeric-code"), v.tr("Revoke code")).Padding(4, 8).Height(32).Disabled(!idle || !current || v.actions.RevokeNumericCode == nil).Clicked() {
			v.numericAction = ActionRevokeNumericCode
			v.actions.RevokeNumericCode(status.Generation)
		}
	})
	switch {
	case status.Committing:
		ui.Text(c, v.tr("Completing pairing")).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
	case status.Redeemed:
		ui.Text(c, v.tr("Code redeemed")).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
	case !current && (status.Generation != 0 || status.Reason == cpace.Reason(app.HubPairingDisabled)):
		message := "Code unavailable"
		switch status.Reason {
		case cpace.Reason(app.HubPairingDisabled):
			message = "Hub numeric pairing is not enabled for this device"
		case "", cpace.ReasonExpired:
			message = "Code expired"
		case cpace.ReasonBudgetExhausted:
			message = "Attempts exhausted"
		case cpace.ReasonRateLimited:
			message = "Try again later"
		case cpace.ReasonStoreFailed:
			message = "Could not save pairing"
		}
		ui.Text(c, v.tr(message)).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
	}
}
