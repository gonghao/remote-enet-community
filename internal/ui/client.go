package ui

import "github.com/egoist/mygo/ui"

func (v *View) clientPage(c *ui.Context) {
	v.phase(c)
	v.messages(c)
	v.card(c.Key("client-actions"), "", "", func() {
		v.nicSelect(c)
		ui.Field(c, v.tr("Invitation code"), func() {
			ui.TextInput(c.Key("invitation-input"), &v.invitation).FillWidth().MinWidth(0).Height(34).Placeholder(v.tr("Paste the operator's renet1: invitation")).Disabled(v.locked())
		}).FillWidth().MinWidth(0).FontSize(11)
		v.startControls(c)
		if v.saved.Client.Peer != "" && !v.locked() {
			ui.Text(c, v.tr("Leave the invitation empty to use the remembered peer.")).FontSize(11).TextColor(muted)
		}
	})
	v.traffic(c)
	v.information(c)
}
