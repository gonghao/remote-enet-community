package ui

import "github.com/egoist/mygo/ui"

const clientFieldLabelSize = 11

func (v *View) clientPage(c *ui.Context) {
	v.phase(c)
	v.messages(c)
	ui.Column(c.Key("client-actions")).FillWidth().MinWidth(0).Gap(8).Children(func() {
		adapter, invitation := v.tr("Adapter"), v.tr("Invitation code")
		adapterWidth, _ := c.MeasureText(0, ui.Span{Text: adapter, Size: clientFieldLabelSize})
		invitationWidth, _ := c.MeasureText(0, ui.Span{Text: invitation, Size: clientFieldLabelSize})
		labelWidth := max(adapterWidth, invitationWidth)
		v.nicSelect(c, adapter, labelWidth)
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			label := ui.Text(c, invitation).FontSize(clientFieldLabelSize).Width(labelWidth).Shrink(0)
			ui.TextInput(c.Key("invitation-input"), &v.invitation).Grow(1).MinWidth(0).Height(32).
				LabelledBy(label).Placeholder(v.tr("Paste the operator's renet1: invitation")).Disabled(v.locked())
		})
		v.startControls(c)
	})
	v.traffic(c)
	v.information(c)
}
