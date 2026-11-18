// Package netpriv keeps privileged network setup separate from GUI identity state.
package netpriv

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"remote-enet/internal/packetio"
	"remote-enet/internal/tunadapter"
)

// Options identifies the bundled helper; an empty path never triggers authorization.
type Options struct{ HelperPath string }

// Status reports read-only platform capabilities.
type Status struct {
	Platform                                         string
	Elevated, HelperAvailable, AuthorizationRequired bool
}

// Inspect only queries capabilities; it never requests authorization.
func Inspect(o Options) Status {
	elevated := isElevated()
	return Status{Platform: runtime.GOOS, Elevated: elevated, HelperAvailable: validHelper(o.HelperPath), AuthorizationRequired: (runtime.GOOS == "darwin" || runtime.GOOS == "windows") && !elevated}
}
func validHelper(path string) bool {
	if strings.IndexFunc(path, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return false
	}
	if !filepath.IsAbs(path) || filepath.Base(path) != "renet-net-helper" {
		return false
	}
	s, err := os.Lstat(path)
	return err == nil && s.Mode().IsRegular() && s.Mode().Perm()&0111 != 0 && s.Mode().Perm()&0022 == 0
}

// Device owns exactly one target and the resources created for that target.
type Device struct {
	packetio.Device
	name  string
	local netip.Prefix
}

// Name identifies the newly owned adapter.
func (d *Device) Name() string { return d.name }

// Local is the host prefix selected by tunadapter.OpenAuto.
func (d *Device) Local() netip.Prefix { return d.local }

// Open does not elevate the GUI or expose its identity state to the helper.
func Open(ctx context.Context, targets []netip.Addr, o Options) (*Device, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(targets) != 1 || !validTarget(targets[0]) {
		return nil, fmt.Errorf("netpriv: one unicast IPv4 target required")
	}
	if runtime.GOOS == "windows" && !isElevated() {
		return nil, fmt.Errorf("netpriv: administrator permission required: %w", os.ErrPermission)
	}
	if runtime.GOOS == "darwin" && !isElevated() {
		if !validHelper(o.HelperPath) {
			return nil, fmt.Errorf("netpriv: bundled helper unavailable: %w", os.ErrPermission)
		}
		return openAuthorized(ctx, targets[0], o)
	}
	a, err := tunadapter.OpenAuto(ctx, targets)
	if err != nil {
		return nil, err
	}
	return &Device{Device: a, name: a.Name(), local: a.Local()}, nil
}
func validTarget(a netip.Addr) bool {
	return a.Is4() && (a.IsGlobalUnicast() || a.IsLinkLocalUnicast()) && a.As4()[0] != 0
}
