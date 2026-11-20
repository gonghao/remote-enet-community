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
	v.clearExpiredNumericCode()
	v.phase(c)
	v.messages(c)
	v.traffic(c)
	v.card(c.Key("operator-actions"), "", "", func() {
		if v.saved.Operator.Mode == "center" {
			v.centerReception(c)
			return
		}
		mode, addressLabel, addr := v.operatorAddress()
		stage, _, color := v.stage()
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Text(c, v.tr("Session controls")).Bold().FontSize(12)
			ui.Text(c, v.tr("One client at a time")).FontSize(11).TextColor(muted)
			ui.Text(c, v.tr(mode)+" · "+v.tr(addressLabel)).FontSize(11)
			ui.Text(c, addr).Grow(1).MinWidth(0).Font("monospace").SingleLine().TruncateMiddle().Tooltip(addr)
			ui.Text(c, v.tr(stage)).Shrink(0).FontSize(11).TextColor(color)
		})
		v.startControls(c)
		if v.hubWaitDenied() {
			v.issuerControls(c)
		}
		ui.Text(c, v.tr("Invite Client")).Bold().FontSize(12)
		ui.Grid(c).FillWidth().Columns(pageColumns(c)).Gap(8).Children(func() {
			v.invitePanel(c.Key("numeric-panel"), "Numeric code · easy to dictate", v.tr("Client enters %s + an 8-digit code.", addr), v.numericCodeControls)
			v.invitePanel(c.Key("invitation-panel"), "Invitation · easy to copy and paste", v.tr("Client pastes the full invitation."), v.invitationControls)
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

func (v *View) operatorAddress() (mode, label, addr string) {
	op := v.saved.Operator
	mode, label, addr = "Direct", "Public endpoint", op.Endpoint
	if op.Mode == "hub" {
		mode, label, addr = "Self-hosted hub", "Share address", op.Hub.Share
		if addr == "" {
			addr = op.Hub.Addr
		}
	}
	if addr == "" {
		addr = "—"
	}
	return
}

const pairingOutputHeight = 32

func (v *View) invitePanel(c *ui.Context, title, hint string, controls func(*ui.Context)) {
	ui.Column(c).Role(ui.RoleGroup).Label(v.tr(title)).FillWidth().MinWidth(0).Padding(4, 8).Gap(4).Border(1, line).Radius(0).Children(func() {
		ui.Text(c, v.tr(title)).Bold().FontSize(12)
		ui.Text(c, hint).FillWidth().SingleLine().TruncateMiddle().Tooltip(hint).FontSize(11)
		controls(c)
	})
}

func (v *View) invitationControls(c *ui.Context) {
	// A single-line editor scrolls long codes without widening its parent.
	invitation := v.state.Invitation
	ui.TextInput(c.Key("invitation-output"), &invitation).Label(v.tr("Generated invitation")).ReadOnly(true).FillWidth().MinWidth(0).Height(pairingOutputHeight).Placeholder(v.tr("No invitation generated"))
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		label := "Generate invitation"
		if v.issuing {
			label = "Generating…"
		}
		if ui.Button(c.Key("issue-invite"), v.tr(label)).Label(v.tr("Generate invitation")).Padding(4, 8).Height(32).Disabled(!v.canInvite()).Clicked() {
			v.issuing = true
			v.actions.IssueInvite()
		}
		if ui.Button(c.Key("copy-invite"), v.tr("Copy invitation")).Label(v.tr("Copy invitation")).Padding(4, 8).Height(32).Disabled(v.state.Invitation == "" || v.actions.Copy == nil).Clicked() {
			v.actions.Copy(v.state.Invitation)
		}
		ui.Text(c, v.tr("Expires after 15 minutes")).TextColor(muted).FontSize(11)
	})
}
