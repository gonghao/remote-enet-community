package tunadapter

import (
	"fmt"
	"os"

	"golang.zx2c4.com/wireguard/tun"
)

func createNative(name string, mtu int) (tun.Device, error) {
	// The upstream auto-name helper otherwise writes an unrelated filesystem path.
	if name == "utun" && os.Getenv("WG_TUN_NAME_FILE") != "" {
		return nil, fmt.Errorf("WG_TUN_NAME_FILE must be unset for automatic utun allocation")
	}
	return tun.CreateTUN(name, mtu)
}
func windowsToolPath(name string) (string, error) {
	return "", fmt.Errorf("Windows tool %q on Darwin", name)
}
