// Package desktopui hosts fixed-role native applications over the shared controller.
package desktopui

import (
	"fmt"

	"remote-enet/internal/settings"
)

type product struct {
	role, name, command                string
	width, height, minWidth, minHeight int
}

func productForRole(role string) (product, error) {
	switch role {
	case settings.RoleClient:
		return product{role: role, name: "Remote ENET Client", command: "renet-client", width: 820, height: 640, minWidth: 680, minHeight: 560}, nil
	case settings.RoleOperator:
		return product{role: role, name: "Remote ENET Operator", command: "renet-operator", width: 1080, height: 760, minWidth: 900, minHeight: 620}, nil
	default:
		return product{}, fmt.Errorf("desktopui: unknown application role")
	}
}
