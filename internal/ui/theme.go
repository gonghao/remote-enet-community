package ui

import (
	"github.com/egoist/mygo/ui"
	"remote-enet/internal/brand"
)

// BackgroundColor fills native windows before their first content frame.
const BackgroundColor = "#f3f5f8"

var (
	canvas         = ui.Hex(BackgroundColor)
	panel          = ui.Hex("#ffffff")
	inputSurface   = ui.Hex("#f8fafc")
	ink            = ui.Hex("#1f2937")
	muted          = ui.Hex("#526174")
	line           = ui.Hex("#bac5d5")
	controlStroke  = ui.Hex("#748296")
	blue           = ui.Hex(brand.Accent)
	green          = ui.Hex("#237a4b")
	amber          = ui.Hex("#8a5700")
	danger         = ui.Hex("#b42332")
	infoSurface    = ui.Hex("#eaf0ff")
	warningSurface = ui.Hex("#fff4d6")
	errorSurface   = ui.Hex("#fdecef")
)

// Theme returns a fresh light theme for both the workspace and startup errors.
// Control outlines and focus indicators use stronger colors than card dividers.
func Theme() *ui.Theme {
	t := ui.LightTheme()
	t.Background, t.Text, t.TextMuted = canvas, ink, muted
	t.Surface, t.SurfaceHover, t.SurfacePressed = inputSurface, ui.Hex("#edf2f7"), ui.Hex("#e1e8f0")
	t.Border = controlStroke
	t.Accent, t.AccentHover, t.AccentPressed = blue, ui.Hex("#1d48b9"), ui.Hex("#173a97")
	t.AccentText = ui.Hex("#ffffff")
	t.Focus, t.Selection = blue, ui.RGBA(35, 86, 216, 0.16)
	t.Scrollbar = ui.Hex("#748296")
	t.Success, t.Warning, t.Danger = green, amber, danger
	t.Radius, t.FontSize, t.Spacing = 0, 12, 4
	t.ScrollbarStyle = ui.ScrollbarStyle{Square: true, TrackWidth: 12, ThumbWidth: 8, MinLength: 24}
	return t
}
