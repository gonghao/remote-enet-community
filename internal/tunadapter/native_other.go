//go:build !linux && !darwin && !windows

package tunadapter

import (
	"fmt"
	"golang.zx2c4.com/wireguard/tun"
)

func createNative(string, int) (tun.Device, error) {
	return nil, fmt.Errorf("unsupported TUN platform")
}
func windowsToolPath(name string) (string, error) {
	return "", fmt.Errorf("Windows tool %q unavailable", name)
}
