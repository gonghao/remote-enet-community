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
					width, height := 1080, 760
					if role == settings.RoleClient {
						width, height = clientWidth, clientHeight
					}
					tt := ui.NewTester(v.Render, width, height)
					tt.SetZoom(zoom)
					for _, value := range []string{"18.0 ms", "517.6 KiB", "263.7 KiB", "2"} {
						if role == settings.RoleClient && zoom > 1 {
							scrollTo(t, tt, value, float32(width), float32(height))
						}
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

func TestClientLongMetricsFitAtMinimumWidth(t *testing.T) {
	for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
		t.Run(language, func(t *testing.T) {
			v := New(settings.Default(), Actions{})
			v.SetLanguage(language)
			v.SetState(clientPreviewState())
			metrics := []ui.Span{
				{Text: "999.9 ms", Size: 24}, {Text: "1023.9 MiB", Size: 24},
				{Text: "1023.8 MiB", Size: 24}, {Text: "1023.9 MiB/s", Size: 10}, {Text: "1023.8 MiB/s", Size: 10},
				{Text: v.tr("Latest measurement"), Size: 10}, {Text: v.tr("Local observations"), Size: 10},
				{Text: v.tr("Received total"), Size: 11}, {Text: v.tr("Active tunnels"), Size: 11},
			}
			widths := make([]float32, len(metrics))
			tt := ui.NewTester(func(c *ui.Context) {
				v.Render(c)
				for i, span := range metrics {
					if span.Size == 24 {
						span.Font, span.Weight, span.Features = metricFontFamily, 700, "calt=0,liga=0"
					}
					widths[i], _ = c.MeasureText(0, span)
				}
			}, clientMinWidth, clientMinHeight)
			for i, span := range metrics {
				r, ok := tt.Find(span.Text)
				if !ok || widths[i] <= 0 || widths[i] > r.W+1 || r.X < 0 || r.X+r.W > clientMinWidth || r.H <= 0 {
					t.Errorf("%q needs %.1f DIP but its box is %+v", span.Text, widths[i], r)
				}
			}
			left, _ := tt.Find("999.9 ms")
			right, _ := tt.Find("1023.9 MiB")
			if left.Y != right.Y || left.X+left.W > right.X {
				t.Fatalf("metric values are not side by side: %+v, %+v", left, right)
			}
		})
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
