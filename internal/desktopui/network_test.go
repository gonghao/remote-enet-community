package desktopui

import (
	"context"
	"errors"
	"net/netip"
	"path/filepath"
	"testing"

	"remote-enet/internal/netpriv"
)

func TestNetworkFactoryFailurePreservesNilInterface(t *testing.T) {
	factory := networkFactory(netpriv.Options{})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	device, err := factory(ctx, []netip.Addr{netip.MustParseAddr("169.254.91.3")})
	if device != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled network factory returned a typed nil or lost cause: %v %v", device, err)
	}
	device, err = factory(t.Context(), []netip.Addr{netip.MustParseAddr("127.0.0.1")})
	if device != nil || err == nil {
		t.Fatalf("invalid network target reached native setup: %v %v", device, err)
	}
}

func TestBundledNetworkHelperStaysWithExecutable(t *testing.T) {
	opts := bundledNetworkOptions()
	if !filepath.IsAbs(opts.HelperPath) || filepath.Base(opts.HelperPath) != "renet-net-helper" {
		t.Fatal("helper path was derived from user settings or the working directory")
	}
	status := netpriv.Status{Platform: "darwin", HelperAvailable: true, AuthorizationRequired: true}
	p := permissionState(status)
	if p.Platform != status.Platform || p.Elevated || !p.HelperAvailable || !p.AuthorizationRequired {
		t.Fatal("network capabilities were inferred instead of copied")
	}
}
