package ui

import (
	"github.com/egoist/mygo/ui"
	"remote-enet/internal/app"
	"remote-enet/internal/locales"
)

func (v *View) hubWaitDenied() bool {
	return v.state.Runtime.LastError == locales.PairingMessage(string(app.HubWaitUnauthorized))
}

func (v *View) operatorPage(c *ui.Context) {
	v.phase(c)
	v.messages(c)
	v.traffic(c)
	v.card(c.Key("operator-actions"), "Session controls", "One client at a time", func() {
		v.startControls(c)
		if v.hubWaitDenied() {
			v.issuerControls(c)
		}
		ui.Grid(c).Columns(pageColumns(c)).FillWidth().Gap(12).Children(func() {
			ui.Column(c).FillWidth().MinWidth(0).Gap(8).Children(func() { v.numericCodeControls(c) })
			ui.Column(c).FillWidth().MinWidth(0).Gap(8).Children(func() { v.invitationControls(c) })
		})
	})
	v.information(c)
	if pageColumns(c) == 2 {
		ui.Grid(c).Columns(2).FillWidth().Gap(12).Children(func() {
			v.linkTest(c, true)
			v.directLink(c)
		})
	} else {
		v.linkTest(c)
		v.directLink(c)
	}
}

func (v *View) invitationControls(c *ui.Context) {
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
		label := "Generate invitation"
		if v.issuing {
			label = "Generating…"
		}
		if ui.Button(c.Key("issue-invite"), v.tr(label)).Label(v.tr("Generate invitation")).Disabled(!v.canInvite()).Clicked() {
			v.issuing = true
			v.actions.IssueInvite()
		}
		if ui.Button(c.Key("copy-invite"), v.tr("Copy invitation")).Label(v.tr("Copy invitation")).Disabled(v.state.Invitation == "" || v.actions.Copy == nil).Clicked() {
			v.actions.Copy(v.state.Invitation)
		}
		ui.Text(c, v.tr("Expires after 15 minutes")).Grow(1).MinWidth(0).TextColor(muted).FontSize(11)
	})
	// A single-line editor scrolls long codes without widening its parent.
	invitation := v.state.Invitation
	ui.TextInput(c.Key("invitation-output"), &invitation).Label(v.tr("Generated invitation")).ReadOnly(true).FillWidth().MinWidth(0).Height(34).Placeholder(v.tr("No invitation generated"))
}
