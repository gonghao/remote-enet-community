package ui

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/settings"
)

func TestMetricFontIsEmbeddedAndMonospaced(t *testing.T) {
	found := false
	for _, family := range ui.FontFamilies() {
		if family.Name == metricFontFamily {
			found = family.Registered && family.Monospaced
		}
	}
	if !found {
		t.Fatal("metric font must come from the embedded monospaced family")
	}
	glyphs := ui.Shape("0123456789.", ui.Font{Family: metricFontFamily, Size: 24, Weight: 700, Features: "calt=0,liga=0"})
	if len(glyphs) != 11 || glyphs[0].Advance <= 0 {
		t.Fatal("metric font did not shape numeric glyphs")
	}
	for _, glyph := range glyphs {
		if math.Abs(float64(glyph.Advance-glyphs[0].Advance)) > 1.0/64 {
			t.Fatal("numeric advance changes between glyphs")
		}
	}
}

func TestMetricValuesStayCanonicalAcrossLanguages(t *testing.T) {
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
			for _, zoom := range []float32{1, 1.5} {
				t.Run(fmt.Sprintf("%s/%s/%g", tag, role, zoom), func(t *testing.T) {
					cfg := operatorConfig()
					cfg.Role = role
					v := New(cfg, Actions{})
					v.SetLanguage(tag)
					v.SetState(previewState())
					width := 1080
					if role == settings.RoleClient {
						width = 820
					}
					tt := ui.NewTester(v.Render, width, 760)
					tt.SetZoom(zoom)
					for _, value := range []string{"18.0 ms", "517.6 KiB", "263.7 KiB", "2"} {
						r, ok := tt.Find(value)
						if !ok || r.W <= 0 || r.H <= 0 || r.X < 0 || r.X+r.W > float32(width)+1 {
							t.Fatalf("metric value is translated or clipped: %q %+v", value, r)
						}
						var advance float32
						for _, glyph := range ui.Shape(value, ui.Font{Family: metricFontFamily, Size: 24, Weight: 700, Features: "calt=0,liga=0"}) {
							advance += glyph.Advance
						}
						if advance*zoom > r.W+1 {
							t.Fatalf("metric value exceeds its available width: %q", value)
						}
					}
					for _, label := range []string{"Peer RTT", "Received total", "Sent total", "Active tunnels"} {
						if !tt.HasText(v.tr(label)) {
							t.Fatalf("localized metric label missing: %s", label)
						}
					}
				})
			}
		}
	}
}

func TestMetricFontPreview(t *testing.T) {
	dir := os.Getenv("RENET_METRIC_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set RENET_METRIC_PREVIEW_DIR to export metric font samples")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	views := make([]*View, 0, 3)
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		v := New(operatorConfig(), Actions{})
		v.SetLanguage(tag)
		v.SetState(previewState())
		views = append(views, v)
	}
	for _, zoom := range []float32{1, 1.5} {
		tt := ui.NewTester(func(c *ui.Context) {
			c.SetTheme(Theme())
			ui.Column(c).Fill().Padding(24).Gap(16).Background(canvas).Children(func() {
				ui.Text(c, "Pixel Code Bold / 700").FontSize(20).Bold().TextColor(ink)
				for i, label := range []string{"English", "简体中文", "繁體中文"} {
					ui.Column(c.Key(label)).FillWidth().Gap(8).Children(func() {
						ui.Text(c, label).FontSize(13).Bold().TextColor(muted)
						views[i].traffic(c)
					})
				}
			})
		}, int(1080*zoom), int(540*zoom))
		tt.SetZoom(zoom)
		writePreview(t, tt, filepath.Join(dir, fmt.Sprintf("metric-font-%g.png", zoom)))
	}
}
