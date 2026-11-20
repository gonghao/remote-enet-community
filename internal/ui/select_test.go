package ui

import (
	"image/color"
	"slices"
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/settings"
)

// Sample the trailing interior, excluding the field's label and border.
func dropdownIcon(tt *ui.Tester, bounds ui.Rect) []color.RGBA {
	frame := tt.Image()
	right, mid := int(bounds.X+bounds.W), int(bounds.Y+bounds.H/2)
	var pixels []color.RGBA
	for y := mid - 8; y < mid+8; y++ {
		for x := right - 32; x < right-2; x++ {
			pixels = append(pixels, frame.RGBAAt(x, y))
		}
	}
	return pixels
}

func TestDropdownsUseThemedChevron(t *testing.T) {
	for _, tc := range []struct {
		role  string
		page  page
		label string
		width int
	}{
		{settings.RoleClient, settingsPage, "Interface language", clientWidth},
		{settings.RoleClient, sessionPage, "Adapter selection", clientWidth},
		{settings.RoleOperator, settingsPage, "Interface language", 1080},
		{settings.RoleOperator, settingsPage, "Connection mode", 1080},
		{settings.RoleOperator, capturePage, "Capture direction filter", 1080},
	} {
		t.Run(tc.role+"/"+tc.label, func(t *testing.T) {
			cfg := operatorConfig()
			cfg.Role = tc.role
			v := New(cfg, Actions{SetLanguage: func(string) {}, Save: func(settings.Config) {}})
			v.page = tc.page
			tt := ui.NewTester(v.Render, tc.width, 900)
			field, ok := tt.AccessByRole(ui.RolePopUpButton, tc.label)
			if !ok {
				t.Fatalf("missing dropdown %q", tc.label)
			}
			reference := ui.NewTester(func(c *ui.Context) {
				c.SetTheme(Theme())
				value := "Option"
				ui.Select(c, &value, []string{value}).Label("Reference").Width(240).Height(field.Bounds.H)
			}, 300, 100)
			standard, ok := reference.AccessByRole(ui.RolePopUpButton, "Reference")
			if !ok {
				t.Fatal("missing reference dropdown")
			}
			want := dropdownIcon(reference, standard.Bounds)
			if !slices.ContainsFunc(want, func(p color.RGBA) bool { return p != want[0] }) {
				t.Fatal("reference arrow is blank")
			}
			if !slices.Equal(dropdownIcon(tt, field.Bounds), want) {
				t.Fatal("arrow differs from the theme Select")
			}
		})
	}
}
