package ui

import (
	"net/netip"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/carside"
	"remote-enet/internal/center"
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
		sel.Trigger.Label(v.tr("Adapter selection")).Disabled(disabled).Grow(1).MinWidth(0).Height(32).Padding(c.Theme().Space(1.5), c.Theme().Space(3.5)).Background(inputSurface).Border(1, controlStroke).Radius(0).
			Tooltip(v.tr("Automatic requires one unambiguous adapter. Fixed selections are kept if an adapter disappears.")).Children(func() {
			ui.Text(c, choices[selected].label).Grow(1).MinWidth(0).SingleLine().TruncateMiddle()
			selectChevron(c)
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

// MyGo keeps Select's chevron private. Match its theme-scaled path for the
// indexed adapter selector, whose labels need middle truncation.
func selectChevron(c *ui.Context) {
	size, color := c.Theme().Space(2.5), c.Theme().TextMuted
	ui.Box(c).Size(size, size).Shrink(0).Draw(func(p *ui.Painter, r ui.Rect) {
		var path ui.Path
		path.MoveTo(r.X+0.1*r.W, r.Y+0.3*r.H).LineTo(r.X+0.5*r.W, r.Y+0.7*r.H).LineTo(r.X+0.9*r.W, r.Y+0.3*r.H)
		p.StrokePath(&path, 1.5, color)
	})
}

func (v *View) operatorForm(c *ui.Context) {
	modes := []string{"direct", "hub", "center"}
	labels := []string{v.tr("Direct"), v.tr("Self-hosted hub"), v.tr("Center")}
	index := max(0, slices.Index(modes, v.draft.Operator.Mode))
	mode := labels[index]
	ui.Field(c, v.tr("Connection mode"), func() {
		if ui.Select(c.Key("hub-mode"), &mode, labels).Label(v.tr("Connection mode")).FillWidth().Height(32).Disabled(v.locked()).Changed() {
			v.draft.Operator.Mode = modes[slices.Index(labels, mode)]
		}
	}).FillWidth()
	if v.draft.Operator.Mode == "center" {
		v.centerForm(c)
		v.accountControls(c)
		return
	}
	if v.draft.Operator.Mode == "hub" {
		ui.Grid(c).Columns(2).FillWidth().Gap(12).Children(func() {
			v.field(c, "Hub address", "host.example:47000", &v.draft.Operator.Hub.Addr, v.locked())
			v.field(c, "Share address", "host.example:47000", &v.draft.Operator.Hub.Share, v.locked())
		})
		v.field(c, "Hub fingerprint", "", &v.draft.Operator.Hub.FP, v.locked())
		v.issuerControls(c)
	} else {

		ui.Grid(c).Columns(2).FillWidth().Gap(12).Children(func() {
			v.field(c, "Listen address", "0.0.0.0:47000", &v.draft.Operator.Listen, v.locked())
			v.field(c, "Public endpoint", "host.example:47000", &v.draft.Operator.Endpoint, v.locked())
		})
	}
}

func (v *View) issuerControls(c *ui.Context) {
	if !v.issuerRequested && v.actions.LoadIssuerEntry != nil {
		v.issuerRequested = true
		v.actions.LoadIssuerEntry()
	}
	v.value(c, "Hub authorization entry", v.issuerEntry)
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		if v.issuerEntry == "" && v.issuerRequested && ui.Button(c.Key("reload-entry"), v.tr("Reload entry")).Disabled(v.actions.LoadIssuerEntry == nil).Clicked() {
			v.actions.LoadIssuerEntry()
		}
		if ui.Button(c.Key("copy-entry"), v.tr("Copy entry")).Disabled(v.issuerEntry == "" || v.actions.Copy == nil).Clicked() {
			v.actions.Copy(v.issuerEntry)
		}
	})
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
	v.value(c, "Center ID", v.saved.Center.ID)
	if len(v.saved.Center.API) == 0 {
		v.value(c, "Center API", "")
	}
	for i, entry := range v.saved.Center.API {
		v.value(c.Key(i), "Center API", entry)
	}
	if ui.Button(c.Key("switch-center"), v.tr("Switch center")).Disabled(v.locked()).Clicked() {
		v.editCenter = !v.editCenter
		v.centerID = v.saved.Center.ID
		v.centerAPI = strings.Join(v.saved.Center.API, "\n")
	}
	if v.editCenter {
		w, _ := c.Size()
		columns := 2
		if w < 400 {
			columns = 1
		}
		ui.Grid(c).Columns(columns).FillWidth().Gap(12).Children(func() {
			v.field(c, "Center ID", "", &v.centerID, v.locked())
			ui.Field(c, v.tr("Center API URLs"), func() {
				ui.TextArea(c.Key("center-api"), &v.centerAPI).Label(v.tr("Center API URLs")).FillWidth().Height(64).Disabled(v.locked())
			}).FillWidth()
		})
		cfg := v.saved
		cfg.Center = settings.Center{ID: v.centerID, API: strings.Fields(v.centerAPI)}
		err := cfg.Validate()
		if err != nil {
			label := "Check the center ID and HTTPS addresses."
			if cfg.Operator.Mode == "center" && cfg.Center.ID == "" && len(cfg.Center.API) == 0 {
				label = "Switch connection mode before clearing the center."
			}
			ui.Text(c, v.tr(label)).FontSize(11).TextColor(c.Theme().Danger)
		}
		ui.Row(c).FillWidth().Gap(8).Children(func() {
			if ui.Button(c.Key("change-center"), v.tr("Change center…")).Disabled(v.locked() || err != nil || cfg.Equal(v.saved) || v.actions.SetCenter == nil).Clicked() {
				v.actions.SetCenter(cfg.Center)
			}
		})
	}
	if ui.Button(c.Key("check-center"), v.tr("Check center")).Disabled(v.saved.Center.ID == "" || v.checkingCenter || v.actions.CheckCenter == nil).Clicked() {
		v.checkingCenter = true
		v.centerResults = nil
		v.actions.CheckCenter()
	}
	if v.checkingCenter {
		ui.Text(c, v.tr("Checking center…")).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
	}
	for i, result := range v.centerResults {
		label := "Center unreachable"
		switch result.Status {
		case center.Verified:
			label = "Center verified"
		case center.IDMismatch:
			label = "Center ID mismatch"
		}
		v.value(c.Key(i), label, result.Entry)
	}
}

// SetCenterResults accepts observations only for the currently saved center.
func (v *View) SetCenterResults(cfg settings.Center, results []center.EntryResult) {
	if cfg.ID == v.saved.Center.ID && slices.Equal(cfg.API, v.saved.Center.API) {
		v.centerResults = slices.Clone(results)
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
