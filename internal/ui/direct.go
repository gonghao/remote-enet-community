package ui

import (
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
)

func (v *View) directLink(c *ui.Context) {
	v.card(c.Key("direct-link"), "Head unit direct link", "Access the head unit at its own IP", func() {
		d := v.state.Runtime.Direct
		label := "Disabled"
		switch d.Phase {
		case app.DirectDiscovering:
			label = "Discovering head unit…"
		case app.DirectStarting:
			label = "Preparing direct access…"
		case app.DirectReady:
			label = "Ready"
		case app.DirectStopping:
			label = "Stopping…"
		case app.DirectFailed:
			label = "Unavailable"
		}
		ui.Grid(c).Columns(2).FillWidth().Gap(8).Children(func() {
			v.value(c, "State", v.tr(label))
			v.value(c, "Head unit IPv4", address(d.Address))
		})
		connected := v.state.Running && !v.state.Stopping && v.state.Runtime.Phase == app.PhaseConnected
		busy := v.state.DirectWorking || v.directStarting || v.directStopping
		ui.Row(c).FillWidth().Gap(8).Children(func() {
			if ui.Button(c.Key("enable-direct"), v.tr("Enable MGU")).Label(v.tr("Enable MGU direct link")).Grow(1).Height(32).Disabled(!connected || busy || d.Phase == app.DirectReady || v.actions.ConnectMGU == nil).Clicked() {
				v.directStarting = true
				v.actions.ConnectMGU()
			}
			if v.actions.ConnectEVO != nil && ui.Button(c.Key("enable-evo"), v.tr("Enable EVO")).Label(v.tr("Enable EVO direct link")).Grow(1).Height(32).Disabled(!connected || busy || d.Phase == app.DirectReady).Clicked() {
				v.directStarting = true
				v.actions.ConnectEVO()
			}
			active := d.Phase == app.DirectReady || d.Phase == app.DirectDiscovering || d.Phase == app.DirectStarting || d.Phase == app.DirectFailed || v.directStarting
			if ui.Button(c.Key("disable-direct"), v.tr("Disable")).Label(v.tr("Disable head unit direct link")).Grow(1).Height(32).Disabled(!connected || !active || v.directStopping || v.actions.DisconnectMGU == nil).Clicked() {
				v.directStopping = true
				v.actions.DisconnectMGU()
			}
		})
		if hint := v.permissionHint(); hint != "" {
			ui.Text(c, v.tr(hint)).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
		}
		if d.LastError != "" {
			ui.Text(c, v.tr("Direct access could not be prepared. Check vehicle support, permissions, and routes.")).FontSize(11).TextColor(amber).Role(ui.RoleAlert)
		} else {
			ui.Text(c, v.tr("EVO checks known SSH addresses. MGU queries the head unit address. Your SSH client handles authentication.")).FontSize(10).TextColor(muted)
		}
	})
}

func (v *View) permissionHint() string {
	p := v.state.Permission
	if p.Elevated {
		return "Administrator access is available."
	}
	switch p.Platform {
	case "darwin":
		if p.HelperAvailable && p.AuthorizationRequired {
			return "System authorization is requested when enabling direct access."
		}
		return "The network helper is missing. Use the complete Operator app."
	case "windows":
		return "Direct access requires administrator permission. Restart Operator with Run as administrator."
	case "linux":
		return "Direct access requires system network configuration permission."
	default:
		return ""
	}
}
