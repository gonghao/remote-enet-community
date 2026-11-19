package ui

import (
	"net/netip"
	"strings"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/carside"
	"remote-enet/internal/settings"
)

func (v *View) field(c *ui.Context, label, placeholder string, value *string, disabled bool) {
	ui.Field(c, v.tr(label), func() {
		ui.TextInput(c.Key(label), value).FillWidth().MinWidth(0).Height(32).Background(inputSurface).FontSize(12).Placeholder(v.tr(placeholder)).Disabled(disabled)
	}).FillWidth().MinWidth(0).FontSize(12)
}

type nicChoice struct {
	name, address, label string
}

func (v *View) nicChoices() ([]nicChoice, int) {
	choices := []nicChoice{{label: v.tr("Automatic")}}
	selected := 0
	found := v.draft.Client.NICName == "" && v.draft.Client.NICAddr == ""
	for _, nic := range v.state.NICs {
		if !nic.Address.Is4() {
			continue
		}
		choices = append(choices, nicChoice{nic.Name, nic.Address.String(), nicLabel(nic)})
		if nic.Name == v.draft.Client.NICName && nic.Address.String() == v.draft.Client.NICAddr {
			selected, found = len(choices)-1, true
		}
	}
	if !found {
		name, address := v.draft.Client.NICName, v.draft.Client.NICAddr
		label := name
		if label == "" {
			label = v.tr("Any adapter")
		}
		if address != "" {
			label += " · " + address
		} else {
			label += " · " + v.tr("automatic IPv4")
		}
		choices = append(choices, nicChoice{name, address, label + " " + v.tr("(saved selection)")})
		selected = len(choices) - 1
	}
	return choices, selected
}

func nicLabel(nic carside.NIC) string {
	return nic.Name + " · " + nic.Address.String()
}

func (v *View) nicSelect(c *ui.Context, label string, labelWidth float32) {
	choices, selected := v.nicChoices()
	ui.Row(c).FillWidth().MinWidth(0).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(clientFieldLabelSize).Width(labelWidth).Shrink(0)
		sel := ui.SelectBase(c.Key("adapter"), &selected)
		disabled := v.locked() || v.submitted != nil || v.actions.Save == nil
		sel.Trigger.Label(v.tr("Adapter selection")).Disabled(disabled).Grow(1).MinWidth(0).Height(32).Padding(4, 8).Background(inputSurface).Border(1, controlStroke).Radius(0).
			Tooltip(v.tr("Automatic requires one unambiguous adapter. Fixed selections are kept if an adapter disappears.")).Children(func() {
			ui.Text(c, choices[selected].label).Grow(1).MinWidth(0).SingleLine().TruncateMiddle()
			ui.Text(c, "⌄").TextColor(muted)
		})
		sel.Popup(func(popup ui.Element) {
			popup.Padding(4).Background(panel).Border(1, line).Radius(0).MaxHeight(240)
			ui.Scroll(c).FillWidth().MaxHeight(230).Children(func() {
				for i, choice := range choices {
					item := sel.Item(i).Padding(8, 12).Radius(0)
					if item.Highlighted() {
						item.Background(c.Theme().Accent).TextColor(c.Theme().AccentText)
					}
					item.Children(func() { ui.Text(c, choice.label).SingleLine().TruncateMiddle() })
				}
			})
		})
		if !disabled && sel.Trigger.Changed() {
			choice := choices[selected]
			v.draft.Client.NICName, v.draft.Client.NICAddr = choice.name, choice.address
			cfg := v.saved
			cfg.Client.NICName, cfg.Client.NICAddr = choice.name, choice.address
			v.submitted = &cfg
			v.savingAdapter = true
			v.actions.Save(cfg)
		}
		refresh := v.tr("Refresh adapters")
		if ui.Button(c.Key("refresh-nics"), "↻").Label(refresh).Tooltip(refresh).Size(32, 32).Padding(0).Shrink(0).
			Disabled(v.locked() || v.refreshing || v.actions.Refresh == nil).Clicked() {
			v.refreshing = true
			v.actions.Refresh()
		}
	})
}

func (v *View) operatorForm(c *ui.Context) {
	hubMode := v.draft.Operator.Mode == "hub"
	ui.Field(c, v.tr("Connection mode"), func() {
		labels := []string{v.tr("Direct"), v.tr("Self-hosted hub")}
		mode := labels[0]
		if hubMode {
			mode = labels[1]
		}
		if ui.Select(c.Key("hub-mode"), &mode, labels).Label(v.tr("Connection mode")).FillWidth().Height(32).Disabled(v.locked()).Changed() {
			hubMode = mode == labels[1]
		}
	}).FillWidth()
	if hubMode {
		v.draft.Operator.Mode = "hub"
		v.field(c, "Hub address", "host.example:47000", &v.draft.Operator.Hub.Addr, v.locked())
		v.field(c, "Hub fingerprint", "", &v.draft.Operator.Hub.FP, v.locked())
		v.field(c, "Share address", "host.example:47000", &v.draft.Operator.Endpoint, v.locked())
		v.issuerControls(c)
	} else {
		v.draft.Operator.Mode = "direct"
		v.draft.Operator.Hub = settings.Hub{}
		ui.Grid(c).Columns(2).FillWidth().Gap(12).Children(func() {
			v.field(c, "Listen address", "0.0.0.0:47000", &v.draft.Operator.Listen, v.locked())
			v.field(c, "Public endpoint", "host.example:47000", &v.draft.Operator.Endpoint, v.locked())
		})
	}
	v.presentationForm(c)
}

func (v *View) issuerControls(c *ui.Context) {
	if !v.issuerRequested && v.actions.LoadIssuerEntry != nil {
		v.issuerRequested = true
		v.actions.LoadIssuerEntry()
	}
	v.value(c, "Hub authorization entry", v.issuerEntry)
	if v.issuerEntry == "" && v.issuerRequested && ui.Button(c.Key("reload-entry"), v.tr("Reload entry")).Disabled(v.actions.LoadIssuerEntry == nil).Clicked() {
		v.actions.LoadIssuerEntry()
	}
	if ui.Button(c.Key("copy-entry"), v.tr("Copy entry")).Disabled(v.issuerEntry == "" || v.actions.Copy == nil).Clicked() {
		v.actions.Copy(v.issuerEntry)
	}
}

func (v *View) presentationForm(c *ui.Context) {
	v.field(c, "Presentation IPv4", "Local IPv4 used by diagnostic tools", &v.draft.Operator.Bind, v.locked())
	ui.Grid(c).Columns(3).FillWidth().Gap(12).Children(func() {
		for i, label := range []string{"Diagnostic TCP", "Control TCP", "Discovery UDP"} {
			v.field(c, label, "1–65535", &v.ports[i], v.locked())
		}
	})
	ui.Checkbox(c.Key("exclusive"), &v.draft.Operator.Exclusive, v.tr("Exclusive diagnostic tool access")).Label(v.tr("Exclusive diagnostic tool access")).Disabled(v.locked())
	ui.Text(c, v.tr("Windows: one diagnostic process at a time. Owner detection is unavailable on other platforms.")).FontSize(11).TextColor(muted)
}

// SetIssuerEntry accepts only the public entry produced by the local operator.
func (v *View) SetIssuerEntry(entry string) { v.issuerEntry = entry }

func (v *View) centerForm(c *ui.Context) {
	v.field(c, "Center ID", "", &v.centerID, v.locked())
	v.field(c, "Center API URLs", "https://…", &v.centerAPI, v.locked())
	cfg := v.saved
	cfg.Center = settings.Center{ID: v.centerID, API: strings.Fields(v.centerAPI)}
	err := cfg.Validate()
	if err != nil {
		ui.Text(c, v.tr("Check the center ID and HTTPS addresses.")).FontSize(11).TextColor(c.Theme().Danger)
	}
	if ui.Button(c.Key("change-center"), v.tr("Change center…")).Disabled(v.locked() || err != nil || cfg.Equal(v.saved) || v.actions.SetCenter == nil).Clicked() {
		v.actions.SetCenter(cfg.Center)
	}
}

func (v *View) value(c *ui.Context, label, text string) ui.Element {
	display := text
	if display == "" {
		display = "—"
	}
	return ui.Row(c).FillWidth().MinWidth(0).MinHeight(20).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, v.tr(label)).FontSize(11).TextColor(muted).Shrink(0)
		ui.Text(c, display).Grow(1).MinWidth(0).SingleLine().TruncateMiddle().TextAlign(ui.End).Font("monospace").FontSize(12).Tooltip(text)
	})
}

func address(addr netip.Addr) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}

func addrPort(addr netip.AddrPort) string {
	if !addr.IsValid() {
		return ""
	}
	return addr.String()
}
