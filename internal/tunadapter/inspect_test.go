package tunadapter

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"

	"golang.zx2c4.com/wireguard/tun"
)

func TestInspectAutoNeverCreatesOrConfiguresDevice(t *testing.T) {
	b := testBackend(nil)
	b.create = func(string, int) (tun.Device, error) { t.Fatal("inspection created device"); return nil, nil }
	b.run = func(context.Context, []string) error { t.Fatal("inspection changed system networking"); return nil }
	b.ready = func(context.Context, tun.Device, netip.Prefix) error {
		t.Fatal("inspection waited for native state")
		return nil
	}
	b.interfaces = func() ([]net.Interface, error) { return []net.Interface{{Name: "occupied", Index: 1}}, nil }
	b.addresses = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("198.18.1.1"), Mask: net.CIDRMask(15, 32)}}, nil
	}
	targets := []netip.Addr{netip.MustParseAddr("169.254.91.3")}
	local, err := inspectAuto(t.Context(), targets, b)
	if err != nil || local != netip.MustParsePrefix("192.0.2.1/32") {
		t.Fatalf("alternate candidate: %s %v", local, err)
	}
	b.routes = func(context.Context) ([]routeEntry, error) {
		return []routeEntry{{Prefix: netip.MustParsePrefix("169.254.91.3/32")}}, nil
	}
	if _, err := inspectAuto(t.Context(), targets, b); !errors.Is(err, ErrTargetConflict) {
		t.Fatalf("target conflict: %v", err)
	}
	b.routes = func(context.Context) ([]routeEntry, error) {
		return []routeEntry{{Prefix: netip.MustParsePrefix("192.0.2.1/32")}, {Prefix: netip.MustParsePrefix("198.51.100.1/32")}, {Prefix: netip.MustParsePrefix("203.0.113.1/32")}}, nil
	}
	if _, err := inspectAuto(t.Context(), targets, b); !errors.Is(err, ErrConflict) {
		t.Fatalf("all local candidates: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b.interfaces = func() ([]net.Interface, error) {
		t.Fatal("pre-canceled inspection enumerated interfaces")
		return nil, nil
	}
	if _, err := inspectAuto(ctx, targets, b); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel: %v", err)
	}
}

func TestInspectAutoPreservesDarwinFallbackRules(t *testing.T) {
	b := testBackend(nil)
	b.os = "darwin"
	b.interfaces = func() ([]net.Interface, error) {
		return []net.Interface{{Name: "en7", Index: 30, Flags: net.FlagUp | net.FlagBroadcast}}, nil
	}
	b.routes = func(context.Context) ([]routeEntry, error) {
		return []routeEntry{{Prefix: netip.MustParsePrefix("169.254.0.0/16"), Gateway: "link#30", Flags: "UCS", Netif: "en7"}}, nil
	}
	targets := []netip.Addr{netip.MustParseAddr("169.254.91.3")}
	if _, err := inspectAuto(t.Context(), targets, b); err != nil {
		t.Fatal(err)
	}
	b.addresses = func(net.Interface) ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("169.254.1.1"), Mask: net.CIDRMask(16, 32)}}, nil
	}
	if _, err := inspectAuto(t.Context(), targets, b); !errors.Is(err, ErrTargetConflict) {
		t.Fatalf("interface conflict bypassed: %v", err)
	}
}
