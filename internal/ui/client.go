package ui

import (
	"slices"
	"time"

	"github.com/egoist/mygo/ui"
	"remote-enet/internal/centerflow"
)

const clientFieldLabelSize = 11

func (v *View) clientPage(c *ui.Context) {
	v.phase(c)
	v.messages(c)
	ui.Column(c.Key("client-actions")).FillWidth().MinWidth(0).Gap(8).Children(func() {
		adapter, invitation := v.tr("Adapter"), v.tr("Pairing")
		adapterWidth, _ := c.MeasureText(0, ui.Span{Text: adapter, Size: clientFieldLabelSize})
		invitationWidth, _ := c.MeasureText(0, ui.Span{Text: invitation, Size: clientFieldLabelSize})
		labelWidth := max(adapterWidth, invitationWidth)
		v.nicSelect(c, adapter, labelWidth)
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, invitation).FontSize(clientFieldLabelSize).Width(labelWidth).Shrink(0)
			ui.TextInput(c.Key(v.inputGeneration), &v.invitation).Grow(1).MinWidth(0).Height(32).
				Label(v.tr("Invite or address:port and code")).Placeholder(v.tr("Invitation, address:port and code, or an 8-digit numeric code")).Tooltip(v.tr("Invitation, address:port and code, or an 8-digit numeric code")).Disabled(v.locked())
		})
		v.startControls(c)
	})
	if !v.sessionRecovery(c) {
		v.traffic(c)
	}
	v.information(c)
}

func (v *View) clearPairingInput() {
	v.invitation = ""
	// Rotate the editor key so its undo history cannot restore the input.
	v.inputGeneration++
}

func (v *View) sessionRecovery(c *ui.Context) bool {
	if v.actions.CenterSessionCandidates == nil {
		return false
	}
	candidates := v.actions.CenterSessionCandidates()
	if len(candidates) == 0 {
		return false
	}
	slices.SortStableFunc(candidates, func(a, b centerflow.SessionCandidate) int {
		return b.RedeemedAt.Compare(a.RedeemedAt)
	})
	ui.Column(c.Key("session-recovery")).FillWidth().Gap(4).Children(func() {
		ui.Text(c, v.tr("Choose a session to recover")).Bold()
		ui.Scroll(c).FillWidth().MaxHeight(80).Children(func() {
			ui.Column(c).FillWidth().Gap(4).Children(func() {
				for _, candidate := range candidates {
					label := candidate.ShortID + " · " + candidate.RedeemedAt.Local().Format(time.DateTime)
					if ui.Button(c.Key(candidate.ID), label).FillWidth().Disabled(v.actions.ResumeCenterSession == nil || v.locked()).Clicked() {
						v.actions.ResumeCenterSession(candidate.ID)
					}
				}
			})
		})
	})
	return true
}
