package ui

import (
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/settings"
)

func (v *View) canMarkCapture() bool {
	return v.role == settings.RoleOperator && v.actions.MarkCapture != nil && v.state.Running && !v.state.Stopping && !v.stopping && v.state.Capture.Active && !v.state.Capture.Stopping && !v.captureStopping
}

// InsertCaptureMarker belongs to the UI thread, including global hotkey callbacks.
// The callback must admit the marker immediately without waiting for file I/O.
func (v *View) InsertCaptureMarker() bool {
	if !v.canMarkCapture() || !v.actions.MarkCapture(v.captureMarkerLabel) {
		return false
	}
	v.captureMarkerLabel = ""
	return true
}

func (v *View) captureMarkerBar(c *ui.Context) {
	if v.role != settings.RoleOperator || v.actions.MarkCapture == nil || !v.state.Capture.Active {
		return
	}
	ui.Row(c.Key("marker-bar")).FillWidth().Padding(8, 16).Gap(12).AlignItems(ui.Center).Background(infoSurface).Children(func() {
		ui.Column(c).Grow(1).MinWidth(0).Children(func() {
			ui.Text(c, v.tr("HSFZ recording · markers %d", v.state.Capture.Markers)).FontSize(12).Bold()
			hint := "Click Insert marker before your next tool action."
			if v.state.CaptureMarkerShortcut != "" {
				hint = v.tr("%s · global shortcut, including while other apps are focused", captureText(v.state.CaptureMarkerShortcut, 64))
			} else if v.state.CaptureMarkerShortcutUnavailable {
				hint = "Global marker shortcut unavailable · use Insert marker"
			}
			ui.Text(c, v.tr(hint)).FontSize(11).FillWidth().MinWidth(0)
		})
		if ui.Button(c.Key("insert-marker"), v.tr("Insert marker")).Label(v.tr("Insert HSFZ marker")).KeyShortcuts(ui.Cmd|ui.Shift, ui.KeyM).Disabled(!v.canMarkCapture()).Clicked() {
			v.InsertCaptureMarker()
		}
	})
}
