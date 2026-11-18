package ui

import (
	_ "embed"
	"fmt"

	"github.com/egoist/mygo/ui"
)

const metricFontFamily = "Pixel Code"

//go:embed assets/fonts/pixel-code/PixelCode-Bold.otf
var metricFontData []byte

func init() {
	if err := ui.RegisterFont(metricFontData, metricFontFamily); err != nil {
		panic(fmt.Errorf("register metric font: %w", err))
	}
}
