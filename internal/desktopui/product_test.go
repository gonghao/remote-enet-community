package desktopui

import (
	"testing"

	"remote-enet/internal/settings"
)

func TestInitialWindowSize(t *testing.T) {
	for _, test := range []struct {
		name, role            string
		workWidth, workHeight int
		wantWidth, wantHeight int
	}{
		{"fits", settings.RoleClient, 1200, 900, 820, 640},
		{"short work area", settings.RoleClient, 1093, 580, 820, 564},
		{"below the minimum", settings.RoleClient, 400, 450, 680, 560},
		{"zero work area", settings.RoleClient, 0, 0, 820, 640},
		{"invalid work area", settings.RoleClient, 430, -1, 820, 640},
		{"operator fits", settings.RoleOperator, 1200, 900, 1080, 760},
		{"operator shrinks", settings.RoleOperator, 1024, 720, 1008, 704},
	} {
		t.Run(test.name, func(t *testing.T) {
			product, err := productForRole(test.role)
			if err != nil {
				t.Fatal(err)
			}
			width, height := product.initialSize(test.workWidth, test.workHeight)
			if width != test.wantWidth || height != test.wantHeight {
				t.Fatalf("initial size = %dx%d, want %dx%d", width, height, test.wantWidth, test.wantHeight)
			}
		})
	}
}
