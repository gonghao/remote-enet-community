package ui

import (
	"strings"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/brand"
	"remote-enet/internal/settings"
)

var brandMark = func() ui.ImageSource {
	source, err := ui.DecodeImage(brand.PNG(32, false, false))
	if err != nil {
		panic(err)
	}
	return source
}()

type page uint8

const (
	sessionPage page = iota
	settingsPage
	logPage
	capturePage
	checksPage
)

// Render builds native controls; the host owns window sizing and invalidation.
func (v *View) Render(c *ui.Context) {
	c.SetTheme(Theme())
	ui.Column(c).Fill().Children(func() {
		v.header(c)
		v.captureMarkerBar(c)
		ui.Scroll(c.Key(v.page)).Grow(1).FillWidth().MinHeight(0).Children(func() {
			ui.Column(c).FillWidth().Padding(16).Gap(12).Children(func() {
				switch v.page {
				case settingsPage:
					v.settingsPage(c)
				case capturePage:
					if v.role == settings.RoleOperator {
						v.capturePage(c)
					}
				case logPage:
					v.pageTitle(c, "Session log", "Recent runtime events · newest first")
					v.checksEntry(c)
					if v.role == settings.RoleOperator {
						if ui.Button(c.Key("open-capture"), v.tr("HSFZ capture")).Label(v.tr("Open HSFZ capture")).Clicked() {
							v.page = capturePage
						}
					}
					ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
						if ui.Button(c.Key("copy-diagnostics"), v.tr("Copy diagnostics")).Label(v.tr("Copy diagnostic report")).Disabled(v.actions.CopyDiagnostics == nil).Clicked() {
							v.actions.CopyDiagnostics()
						}
						ui.Text(c, v.tr("Copies app status and measurements without addresses or credentials.")).Grow(1).MinWidth(0).FontSize(11).TextColor(muted)
					})
					v.messages(c)
					v.events(c)
				case checksPage:
					v.checksPage(c)
				default:
					if v.role == settings.RoleClient {
						v.clientPage(c)
					} else {
						v.operatorPage(c)
					}
				}
			})
		})
	})
}

func (v *View) roleTitle() string {
	if v.role == settings.RoleOperator {
		return "Operator"
	}
	return "Client"
}

func (v *View) header(c *ui.Context) {
	w, _ := c.Size()
	ui.Row(c).FillWidth().Padding(8, 16).Gap(8).AlignItems(ui.Center).Background(panel).BorderWidth(0, 0, 1, 0).BorderColor(line).Children(func() {
		ui.Image(c.Key("brand-mark"), brandMark).Size(32, 32).Shrink(0)
		title := "Remote ENET · " + v.tr(v.roleTitle())
		if w < 600 {
			title = v.roleTitle()
		}
		ui.Text(c, v.tr(title)).Grow(1).MinWidth(0).SingleLine().FontSize(16).Bold()
		if v.page != sessionPage {
			if ui.Button(c.Key("back"), v.tr("Back")).Label(v.tr("Back to session")).Clicked() {
				v.page = sessionPage
			}
		}
		if ui.Button(c.Key("settings"), v.tr("Settings")).Label(v.tr("Settings")).Disabled(v.page == settingsPage).Clicked() {
			v.page = settingsPage
		}
		if ui.Button(c.Key("log"), v.tr("Log")).Label(v.tr("Log")).Disabled(v.page == logPage).Clicked() {
			v.page = logPage
		}
		if ui.Button(c.Key("quit"), v.tr("Quit")).Label(v.tr("Quit Remote ENET")).Disabled(v.actions.Quit == nil).Clicked() {
			v.actions.Quit()
		}
	})
}

func (v *View) card(c *ui.Context, title, subtitle string, fn func()) {
	ui.Column(c).FillWidth().MinWidth(0).Padding(12).Gap(12).Background(panel).Border(1, line).Radius(0).Children(func() {
		if title != "" {
			ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
				ui.Text(c, v.tr(title)).FontSize(12).Bold()
				if subtitle != "" {
					ui.Text(c, v.tr(subtitle)).Grow(1).MinWidth(0).FontSize(11).TextColor(muted)
				}
			})
		}
		fn()
	})
}

func (v *View) pageTitle(c *ui.Context, title, subtitle string) {
	ui.Column(c).FillWidth().MinWidth(0).Gap(4).Children(func() {
		ui.Text(c, v.tr(title)).FontSize(18).Bold()
		ui.Text(c, v.tr(subtitle)).TextColor(muted).FontSize(11)
	})
}

func (v *View) messages(c *ui.Context) {
	if v.state.Error != "" {
		v.banner(c.Key("error"), "Action failed", v.tr(v.state.Error), c.Theme().Danger, errorSurface, ui.RoleAlert)
	}
	if v.state.Notice != "" {
		v.banner(c.Key("notice"), "Update", v.tr(v.state.Notice), blue, infoSurface, ui.RoleStatus)
	}
	if v.state.Runtime.LastError != "" && v.state.Runtime.LastError != v.state.Error {
		v.banner(c.Key("runtime-error"), "Runtime message", v.state.Runtime.LastError, amber, warningSurface, ui.RoleStatus)
	}
}

func (v *View) banner(c *ui.Context, title, message string, text, fill ui.Color, role ui.Role) {
	ui.Row(c).FillWidth().Padding(8, 12).Gap(8).Radius(0).Background(fill).TextColor(text).Role(role).Children(func() {
		ui.Text(c, v.tr(title)).Bold().FontSize(11).Shrink(0)
		ui.Text(c, message).Grow(1).MinWidth(0).FontSize(11)
	})
}

func (v *View) startControls(c *ui.Context) {
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		if v.role == settings.RoleClient {
			label := "Connect"
			if v.connecting {
				label = "Starting…"
			}
			if ui.PrimaryButton(c.Key("connect"), v.tr(label)).Label(v.tr("Connect client")).Grow(1).Height(36).Disabled(!v.canConnect()).Clicked() {
				v.connecting = true
				v.actions.Connect(strings.TrimSpace(v.invitation))
			}
		} else {
			label := "Start listening"
			if v.listening {
				label = "Starting…"
			}
			if ui.PrimaryButton(c.Key("listen"), v.tr(label)).Label(v.tr("Start operator")).Grow(1).Height(36).Disabled(!v.canListen()).Clicked() {
				v.listening = true
				v.actions.Listen()
			}
		}
		label := "Stop relay"
		if v.role == settings.RoleOperator {
			label = "End session"
		}
		if v.state.Stopping || v.stopping {
			label = "Stopping…"
		}
		if ui.Button(c.Key("stop"), v.tr(label)).Label(v.tr("Stop runtime")).Grow(1).Height(36).TextColor(c.Theme().Danger).Disabled(!v.canStop()).Clicked() {
			v.stopping = true
			v.actions.Stop()
		}
	})
	status := "Ready to start with saved settings."
	switch {
	case v.state.Stopping || v.stopping:
		status = "Stopping the session and releasing listeners…"
	case v.locked():
		status = "Runtime active · stop before changing network settings."
	case v.submitted != nil:
		status = "Waiting for settings to be saved…"
	case v.dirty():
		status = "Unsaved changes · open Settings and save before starting."
	case v.role == settings.RoleClient && strings.TrimSpace(v.invitation) == "" && v.saved.Client.Peer == "":
		status = "Paste an invitation from your operator to connect."
	case v.role == settings.RoleOperator && (v.saved.Operator.Endpoint == "" || v.saved.Operator.Bind == ""):
		status = "Open Settings to save a public endpoint and presentation IPv4."
	}
	ui.Text(c, v.tr(status)).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
}

func (v *View) settingsPage(c *ui.Context) {
	v.pageTitle(c, v.roleTitle()+" settings", "Save explicitly · changes apply on the next start")
	v.languageSelect(c)
	v.messages(c)
	if v.locked() {
		ui.Text(c, v.tr("Runtime active · stop before changing network settings.")).TextColor(amber).FontSize(11).Role(ui.RoleStatus)
	}
	if v.role == settings.RoleClient {
		v.card(c.Key("client-settings"), "Vehicle network", "", func() {
			v.nicSelect(c)
			if v.saved.Client.Peer != "" {
				v.value(c, "Remembered peer", v.saved.Client.Peer)
			}
		})
	} else {
		v.card(c.Key("operator-settings"), "Network & presentation", "", func() { v.operatorForm(c) })
	}
	v.card(c.Key("save-settings"), "Preferences", "", func() { v.saveControls(c) })
	v.checksEntry(c)
	if v.role == settings.RoleClient {
		v.linkTest(c)
	}
}

func (v *View) saveControls(c *ui.Context) {
	ui.Checkbox(c.Key("allow-test"), &v.draft.AllowLinkTest, v.tr("Allow peer connection tests")).Label(v.tr("Allow peer connection tests")).Disabled(v.locked())
	cfg, err := v.config()
	if err != nil {
		ui.Text(c, v.validationMessage(err)).FontSize(11).TextColor(c.Theme().Danger).Role(ui.RoleAlert)
	}
	ui.Row(c).FillWidth().Gap(12).AlignItems(ui.Center).Children(func() {
		message, color := "Saved settings", muted
		if v.dirty() {
			message, color = "Unsaved changes", amber
		}
		if v.submitted != nil {
			message, color = "Saving settings…", blue
		}
		ui.Text(c, v.tr(message)).Grow(1).FontSize(11).TextColor(color).Role(ui.RoleStatus)
		if ui.PrimaryButton(c.Key("save"), v.tr("Save settings")).Label(v.tr("Save settings")).Disabled(v.locked() || v.submitted != nil || !v.dirty() || err != nil || v.actions.Save == nil).Clicked() {
			submitted := cfg
			v.submitted = &submitted
			v.actions.Save(cfg)
		}
	})
}

func (v *View) events(c *ui.Context) {
	ui.Column(c).FillWidth().Gap(4).Children(func() {
		if len(v.state.Events) == 0 {
			ui.Text(c, v.tr("No events yet.")).TextColor(muted)
		}
		for i := len(v.state.Events) - 1; i >= max(0, len(v.state.Events)-200); i-- {
			ui.Text(c, v.tr(v.state.Events[i])).FillWidth().Padding(8, 12).Background(panel).Border(1, line).Radius(0).FontSize(12)
		}
	})
}
