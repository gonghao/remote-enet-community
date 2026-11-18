package desktopui

import (
	"context"
	"net/netip"
	"os"
	"path/filepath"

	"remote-enet/internal/app"
	"remote-enet/internal/netpriv"
	renetui "remote-enet/internal/ui"
)

func bundledNetworkOptions() netpriv.Options {
	executable, err := os.Executable()
	if err != nil {
		return netpriv.Options{}
	}
	return netpriv.Options{HelperPath: filepath.Join(filepath.Dir(executable), "renet-net-helper")}
}

func networkFactory(opts netpriv.Options) app.DirectFactory {
	return func(ctx context.Context, targets []netip.Addr) (app.DirectDevice, error) {
		device, err := netpriv.Open(ctx, targets, opts)
		if device == nil {
			return nil, err
		}
		return device, err
	}
}

func permissionState(status netpriv.Status) renetui.Permission {
	return renetui.Permission{Platform: status.Platform, Elevated: status.Elevated, HelperAvailable: status.HelperAvailable, AuthorizationRequired: status.AuthorizationRequired}
}
