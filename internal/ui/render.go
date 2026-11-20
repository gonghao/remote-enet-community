package ui

import (
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/brand"
	"remote-enet/internal/identity"
	"remote-enet/internal/locales"
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
	peerDetailsPage
)

// Render builds native controls; the host owns window sizing and invalidation.
func (v *View) Render(c *ui.Context) {
	c.SetTheme(Theme())
	accountPage := (v.page == sessionPage || v.page == settingsPage && v.draft.Operator.Mode == "center") && v.role == settings.RoleOperator && v.saved.Operator.Mode == "center"
	if accountPage && (!v.enteredCenter || v.lastCenterPage != v.page) && !v.locked() && !v.accountPending && v.actions.RefreshCenter != nil {
		v.enteredCenter = true
		v.lastCenterPage = v.page
		v.refreshCenter()
	}
	if !accountPage {
		v.enteredCenter = false
	}
	ui.Column(c).Fill().Border(1, line).Radius(0).Children(func() {
		v.header(c)
		v.captureMarkerBar(c)
		ui.Scroll(c.Key(v.page)).Grow(1).FillWidth().MinHeight(0).Children(func() {
			vertical, horizontal, gap := float32(16), float32(16), float32(12)
			if v.page == sessionPage || v.page == settingsPage {
				vertical, gap = 12, 8
			}
			if v.role == settings.RoleClient && v.page == sessionPage {
				vertical, horizontal, gap = 8, 12, 4
			}
			ui.Column(c).FillWidth().Padding(vertical, horizontal).Gap(gap).Children(func() {
				switch v.page {
				case peerDetailsPage:
					v.pageTitle(c, "Peer details", "")
					v.card(c.Key("peer-id"), "Full peer ID", "", func() {
						ui.Text(c, v.detailsPeer.String()).FillWidth().MinWidth(0).Font("monospace")
						if ui.Button(c.Key("copy-peer-id"), v.tr("Copy full peer ID")).Disabled(v.actions.Copy == nil).Clicked() {
							v.actions.Copy(v.detailsPeer.String())
						}
					})
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
	v.sasDialog(c)
}

func (v *View) roleTitle() string {
	if v.role == settings.RoleOperator {
		return "Operator"
	}
	return "Client"
}

func (v *View) header(c *ui.Context) {
	w, _ := c.Size()
	padding, gap := float32(16), float32(8)
	compact := w < 400
	if compact {
		padding, gap = 8, 4
	}
	button := func(key, label, access string) ui.Element {
		b := ui.Button(c.Key(key), v.tr(label)).Label(v.tr(access)).Shrink(0)
		if compact {
			b.Padding(6, 8)
		}
		return b
	}
	ui.Row(c).FillWidth().Padding(8, padding).Gap(gap).AlignItems(ui.Center).Background(panel).BorderWidth(0, 0, 1, 0).BorderColor(line).DragWindow().Children(func() {
		ui.Image(c.Key("brand-mark"), brandMark).Size(32, 32).Shrink(0)
		title := "Remote ENET · " + v.tr(v.roleTitle())
		if w < 600 {
			title = v.roleTitle()
		}
		ui.Text(c, v.tr(title)).Grow(1).MinWidth(0).SingleLine().TruncateMiddle().FontSize(16).Bold()
		if v.page != sessionPage {
			destination, label := sessionPage, "Back to session"
			if v.page == peerDetailsPage && v.peerDetailsFrom == settingsPage {
				destination, label = settingsPage, "Back to settings"
			}
			if button("back", "Back", label).Clicked() {
				v.page = destination
			}
		}
		if button("settings", "Settings", "Settings").Disabled(v.page == settingsPage).Clicked() {
			v.page = settingsPage
		}
		if button("log", "Log", "Log").Disabled(v.page == logPage).Clicked() {
			v.page = logPage
		}
		if button("quit", "Quit", "Quit Remote ENET").Disabled(v.actions.Quit == nil).Clicked() {
			v.actions.Quit()
		}
	})
}

// Split settings and diagnostic cards only when each can use nearly 480 logical DIP.
const twoColumnWidth = 1000

func pageColumns(c *ui.Context) int {
	w, _ := c.Size()
	if w >= twoColumnWidth {
		return 2
	}
	return 1
}

func (v *View) card(c *ui.Context, title, subtitle string, fn func()) {
	gap := float32(12)
	if v.page == sessionPage || v.page == settingsPage {
		gap = 8
	}
	vertical := float32(12)
	if v.page == sessionPage && v.role == settings.RoleOperator {
		vertical, gap = 8, 4
	}
	ui.Column(c).FillWidth().MinWidth(0).Padding(vertical, 12).Gap(gap).Background(panel).Border(1, line).Radius(0).Children(func() {
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
		if subtitle != "" {
			ui.Text(c, v.tr(subtitle)).TextColor(muted).FontSize(11)
		}
	})
}

func (v *View) messages(c *ui.Context) {
	errText := v.tr(v.state.Error)
	if s := v.state.Center; s.SASID != 0 && !time.Now().Before(s.SASDeadline) {
		errText = v.tr("Short code was not confirmed in time")
	}
	runtimeError := locales.RuntimeErrorText(v.language, v.state.Runtime.LastError)
	updates := func() {
		if errText != "" {
			v.banner(c.Key("error"), "Action failed", errText, c.Theme().Danger, errorSurface, ui.RoleAlert)
		}
		if v.state.Notice != "" {
			v.banner(c.Key("notice"), "Update", v.tr(v.state.Notice), blue, infoSurface, ui.RoleStatus)
		}
	}
	runtimeMessage := func() {
		if runtimeError != "" && runtimeError != errText {
			v.banner(c.Key("runtime-error"), "Runtime message", runtimeError, amber, warningSurface, ui.RoleStatus)
		}
	}
	if v.page == sessionPage {
		// Operator has room to share a line and needs the vertical space for pairing at 150%.
		if v.role == settings.RoleOperator {
			ui.Row(c.Key("session-messages")).FillWidth().Height(16).Shrink(0).Gap(4).Children(func() {
				updates()
				runtimeMessage()
			})
			return
		}
		ui.Column(c.Key("session-messages")).FillWidth().Height(32).Shrink(0).Children(func() {
			ui.Row(c.Key("updates")).FillWidth().Height(16).Shrink(0).Gap(4).Children(updates)
			ui.Row(c.Key("runtime-message")).FillWidth().Height(16).Shrink(0).Children(runtimeMessage)
		})
		return
	}
	updates()
	runtimeMessage()
}

func (v *View) banner(c *ui.Context, title, message string, text, fill ui.Color, role ui.Role) {
	row := ui.Row(c).Radius(0).Background(fill).TextColor(text).Role(role)
	if v.page == sessionPage {
		row.Grow(1).Basis(0).MinWidth(0).Height(16).Padding(0, 8).Gap(4).AlignItems(ui.Center).Tooltip(message)
	} else {
		row.FillWidth().Padding(8, 12).Gap(8)
	}
	row.Children(func() {
		ui.Text(c, v.tr(title)).Bold().FontSize(11).Shrink(0)
		label := ui.Text(c, message).Grow(1).MinWidth(0).FontSize(11)
		if v.page == sessionPage {
			label.SingleLine().TruncateMiddle()
		}
	})
}

func (v *View) startControls(c *ui.Context) {
	client := v.role == settings.RoleClient
	height := float32(36)
	if client {
		height = 32
	}
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		if client && v.state.Center.Phase == "retry" {
			if ui.PrimaryButton(c.Key("retry"), v.tr("Retry pairing")).Grow(1).Height(height).Disabled(v.actions.RetryPairing == nil || v.retryPending || v.state.Stopping).Clicked() {
				v.retryPending = true
				v.actions.RetryPairing()
			}
		} else if client {
			label := "Connect"
			if v.connecting {
				label = "Starting…"
			}
			if ui.PrimaryButton(c.Key("connect"), v.tr(label)).Label(v.tr("Connect client")).Grow(1).Height(height).Disabled(!v.canConnect()).Clicked() {
				v.connecting = true
				input := strings.TrimSpace(v.invitation)
				v.clearPairingInput()
				v.actions.Connect(input)
			}
		} else {
			label := "Start listening"
			if v.saved.Operator.Mode == "hub" {
				label = "Connect to hub and wait"
			}
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
		if ui.Button(c.Key("stop"), v.tr(label)).Label(v.tr("Stop runtime")).Grow(1).Height(height).TextColor(c.Theme().Danger).Disabled(!v.canStop()).Clicked() {
			v.stopping = true
			v.ClearPairingSecrets()
			if client && v.state.Center.Phase != "" && v.actions.EndReception != nil {
				v.actions.EndReception()
			} else {
				v.actions.Stop()
			}
		}
	})
	message := v.tr("Ready to start with saved settings.")
	switch {
	case v.state.Stopping || v.stopping:
		message = v.tr("Stopping the session and releasing listeners…")
	case v.locked():
		message = v.tr("Runtime active · stop before changing network settings.")
	case v.submitted != nil:
		message = v.tr("Waiting for settings to be saved…")
	case !client:
		message = v.operatorStartHint()
	case v.dirty():
		message = v.tr("Unsaved changes · open Settings and save before starting.")
	case strings.TrimSpace(v.invitation) == "":
		message = v.tr("Leave the pairing input empty to use the remembered peer.")
		if v.saved.Client.Peer == "" {
			return
		}
	}
	text := ui.Text(c, message).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
	if client {
		text.FillWidth().MinWidth(0).SingleLine().Tooltip(message)
	}
}

func (v *View) operatorStartHint() string {
	op := v.saved.Operator
	var missing []string
	addMissing := func(value, label string) {
		if value == "" {
			missing = append(missing, v.tr(label))
		}
	}
	if op.Mode == "hub" {
		addMissing(op.Hub.Addr, "Hub address")
		addMissing(op.Hub.FP, "Hub fingerprint")
	} else {
		addMissing(op.Listen, "Listen address")
		addMissing(op.Endpoint, "Public endpoint")
	}
	addMissing(op.Bind, "Presentation IPv4")
	if len(missing) > 0 {
		return v.tr("Complete these settings: %s", strings.Join(missing, " · "))
	}
	if v.dirty() {
		return v.tr("Unsaved changes · open Settings and save before starting.")
	}
	return v.tr("Ready to start with saved settings.")
}

func (v *View) settingsPage(c *ui.Context) {
	v.pageTitle(c, v.roleTitle()+" settings", "Save explicitly · changes apply on the next start")
	v.messages(c)
	if v.locked() {
		ui.Text(c, v.tr("Runtime active · stop before changing network settings.")).TextColor(amber).FontSize(11).Role(ui.RoleStatus)
	}
	if v.role == settings.RoleOperator {
		ui.Grid(c).Columns(pageColumns(c)).FillWidth().Gap(12).Children(func() {
			v.card(c.Key("connection-settings"), "Connection settings", "", func() { v.operatorForm(c) })
			v.card(c.Key("operator-settings"), "Network & presentation", "", func() { v.presentationForm(c) })
		})
	}
	v.card(c.Key("save-settings"), "Preferences", "", func() {
		ui.Grid(c).Columns(pageColumns(c)).FillWidth().Gap(8).Children(func() {
			ui.Column(c).FillWidth().MinWidth(0).Gap(4).Children(func() { v.languageSelect(c) })
			ui.Column(c).FillWidth().MinWidth(0).Gap(8).Children(func() {
				if v.role == settings.RoleClient && v.saved.Client.Peer != "" {
					peer, _ := identity.ParseFingerprint(v.saved.Client.Peer)
					v.peerValue(c.Key("remembered-peer"), "Remembered peer", peer)
				}
				ui.Checkbox(c.Key("allow-test"), &v.draft.AllowLinkTest, v.tr("Allow peer connection tests")).Label(v.tr("Allow peer connection tests")).Disabled(v.locked())
			})
		})
	})
	if v.role == settings.RoleClient {
		v.card(c.Key("client-center"), "Center", "", func() { v.centerForm(c) })
		v.linkTest(c)
	}
	v.saveControls(c)
}

func (v *View) saveControls(c *ui.Context) {
	cfg, err := v.config()
	if err != nil {
		ui.Text(c, v.validationMessage(err)).FontSize(11).TextColor(c.Theme().Danger).Role(ui.RoleAlert)
	}
	width, _ := c.Size()
	renderStatus := func() {
		message, color := "Saved settings", muted
		if v.dirty() {
			message, color = "Unsaved changes", amber
		}
		if v.submitted != nil {
			message, color = "Saving settings…", blue
		}
		ui.Text(c, v.tr(message)).Grow(1).FontSize(11).TextColor(color).Role(ui.RoleStatus)
	}
	if width < 400 {
		renderStatus()
	}
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
		if width >= 400 {
			renderStatus()
		}
		v.checksEntry(c)
		save := ui.PrimaryButton(c.Key("save"), v.tr("Save settings"))
		if width < 400 {
			save.Padding(4, 8)
		}
		if save.Label(v.tr("Save settings")).Disabled(v.locked() || v.submitted != nil || !v.dirty() || err != nil || v.actions.Save == nil).Clicked() {
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
