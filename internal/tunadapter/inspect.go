package tunadapter

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strings"
)

// ErrTargetConflict identifies an existing address or route covering the target.
var ErrTargetConflict = errors.New("tunadapter: target conflict")

var autoAddresses = []string{"198.18.0.1", "192.0.2.1", "198.51.100.1", "203.0.113.1"}

func checkAvailability(ctx context.Context, c Config, b backend) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	interfaces, err := b.interfaces()
	if err != nil {
		return fmt.Errorf("list interfaces: %w", err)
	}
	for _, iface := range interfaces {
		if iface.Name == c.Name || (b.os == "windows" && strings.EqualFold(iface.Name, c.Name)) {
			return fmt.Errorf("tunadapter: interface %q already exists", c.Name)
		}
		addresses, e := b.addresses(iface)
		if e != nil {
			return fmt.Errorf("addresses of %s: %w", iface.Name, e)
		}
		for _, address := range addresses {
			prefix, e := netip.ParsePrefix(address.String())
			if e != nil {
				return fmt.Errorf("parse interface address: %w", e)
			}
			if prefix.Contains(c.Local.Addr()) {
				return fmt.Errorf("%w: local conflicts with %s", ErrConflict, iface.Name)
			}
			for _, target := range c.Targets {
				if prefix.Contains(target) {
					return fmt.Errorf("%w: target %s conflicts with %s", ErrTargetConflict, target, iface.Name)
				}
			}
		}
	}
	routes, err := b.routes(ctx)
	if err != nil {
		return fmt.Errorf("inspect routes: %w", err)
	}
	for _, route := range routes {
		p := route.Prefix
		if p.Bits() == 0 {
			continue
		}
		if p.Contains(c.Local.Addr()) {
			return fmt.Errorf("%w: local conflicts with route %s (interface=%q flags=%q gateway=%q)", ErrConflict, p, route.Netif, route.Flags, route.Gateway)
		}
		for _, t := range c.Targets {
			if p.Contains(t) && !darwinLinkLocalFallback(b.os, route, t, interfaces) {
				return fmt.Errorf("%w: target %s conflicts with route %s (interface=%q flags=%q gateway=%q)", ErrTargetConflict, t, p, route.Netif, route.Flags, route.Gateway)
			}
		}
	}
	return ctx.Err()
}

// InspectAuto reuses OpenAuto's conflict rules without creating a device or
// changing addresses/routes. A successful observation does not reserve resources.
func InspectAuto(ctx context.Context, targets []netip.Addr) (netip.Prefix, error) {
	return inspectAuto(ctx, targets, systemBackend())
}

func inspectAuto(ctx context.Context, targets []netip.Addr, b backend) (netip.Prefix, error) {
	var err error
	for _, ip := range autoAddresses {
		local := netip.MustParseAddr(ip)
		collision := false
		for _, target := range targets {
			if target == local {
				collision = true
			}
		}
		if collision {
			err = ErrConflict
			continue
		}
		var c Config
		c, err = validate(Config{Local: netip.PrefixFrom(local, 32), Targets: targets}, b.os)
		if err == nil {
			err = checkAvailability(ctx, c, b)
		}
		if err == nil {
			return c.Local, nil
		}
		if !errors.Is(err, ErrConflict) {
			return netip.Prefix{}, err
		}
	}
	return netip.Prefix{}, err
}
