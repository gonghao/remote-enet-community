package ui

import "github.com/egoist/mygo/ui"

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
				Label(v.tr("Invite or address:port and code")).Placeholder(v.tr("Invite or address:port and code")).Disabled(v.locked())
		})
		v.startControls(c)
	})
	v.traffic(c)
	v.information(c)
}

func (v *View) clearPairingInput() {
	v.invitation = ""
	// Rotate the editor key so its undo history cannot restore the input.
	v.inputGeneration++
}
