//go:build !darwin

package netpriv

import (
	"context"
	"fmt"
	"net/netip"
)

func openAuthorized(context.Context, netip.Addr, Options) (*Device, error) {
	return nil, fmt.Errorf("netpriv: authorization unsupported")
}

// RunHelper rejects unsupported platforms before any network setup.
func RunHelper(context.Context, []string) error { return fmt.Errorf("netpriv: helper requires macOS") }
