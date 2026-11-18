package tunadapter

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/netip"
	"path/filepath"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.zx2c4.com/wireguard/tun"
)

func createNative(name string, mtu int) (tun.Device, error) {
	// Wintun can rename a foreign adapter on alias collision. Never give it a
	// predictable alias, even when the caller requests a stable label.
	suffix := make([]byte, 16)
	if _, err := rand.Read(suffix); err != nil {
		return nil, fmt.Errorf("allocate private Wintun name: %w", err)
	}
	return tun.CreateTUNWithRequestedGUID(fmt.Sprintf("%s-%x", name, suffix), nil, mtu)
}
func windowsToolPath(name string) (string, error) {
	directory, err := windows.GetSystemDirectory()
	if err != nil {
		return "", err
	}
	return filepath.Join(directory, name+".exe"), nil
}

func init() {
	nativeReadiness = func(ctx context.Context, device tun.Device, local netip.Prefix) error {
		native := device.(*tun.NativeTun)
		return waitAddressReady(ctx, native.LUID(), local, addressReadyTimeout, windowsAddressObservations, pauseAddressPoll)
	}
}

func windowsAddressObservations(ctx context.Context) ([]addressObservation, error) {
	size := uint32(15000)
	const maximum = 1 << 20
	for range 3 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if size == 0 || size > maximum {
			return nil, fmt.Errorf("adapter address buffer exceeds %d bytes", maximum)
		}
		buffer := make([]byte, size)
		head := (*windows.IpAdapterAddresses)(unsafe.Pointer(&buffer[0]))
		flags := uint32(windows.GAA_FLAG_SKIP_ANYCAST | windows.GAA_FLAG_SKIP_MULTICAST | windows.GAA_FLAG_SKIP_DNS_SERVER | windows.GAA_FLAG_INCLUDE_ALL_INTERFACES)
		// This synchronous Win32 query has no cancellation API. Check ctx on both
		// sides; never spawn abandoned query goroutines to simulate cancellation.
		err := windows.GetAdaptersAddresses(windows.AF_INET, flags, 0, head, &size)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, windows.ERROR_BUFFER_OVERFLOW) {
			continue
		}
		if errors.Is(err, windows.ERROR_NO_DATA) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("GetAdaptersAddresses: %w", err)
		}
		var rows []addressObservation
		if size == 0 {
			return rows, nil
		}
		for adapter := head; adapter != nil; adapter = adapter.Next {
			for address := adapter.FirstUnicastAddress; address != nil; address = address.Next {
				ip, ok := netip.AddrFromSlice(address.Address.IP())
				if !ok || !ip.Is4() {
					continue
				}
				rows = append(rows, addressObservation{luid: adapter.Luid, prefix: netip.PrefixFrom(ip, int(address.OnLinkPrefixLength)), up: adapter.OperStatus == windows.IfOperStatusUp, dad: address.DadState})
			}
		}
		runtime.KeepAlive(buffer)
		return rows, nil
	}
	return nil, fmt.Errorf("adapter address buffer changed repeatedly")
}
