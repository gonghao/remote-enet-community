package tunadapter

import (
	"fmt"

	"golang.org/x/sys/unix"
	"golang.zx2c4.com/wireguard/tun"
)

func createNative(name string, mtu int) (tun.Device, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	owned := true
	defer func() {
		if owned {
			_ = unix.Close(fd)
		}
	}()
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return nil, err
	}
	// Kernel exclusion prevents a check/create race from attaching an existing TUN.
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI | unix.IFF_TUN_EXCL)
	if err = unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		return nil, fmt.Errorf("exclusive TUN creation: %w", err)
	}
	device, _, err := tun.CreateUnmonitoredTUNFromFD(fd)
	if err != nil {
		return nil, err
	}
	owned = false
	_ = mtu // Configured through the owned interface after native creation.
	return device, nil
}
func windowsToolPath(name string) (string, error) {
	return "", fmt.Errorf("Windows tool %q on Linux", name)
}
