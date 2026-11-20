package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/secure"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
)

// ClientHooks allows deterministic network-change tests. Nil functions use
// system enumeration, UDP discovery and timers. All hooks must honor ctx;
// Wait can be called concurrently by the connection monitor and retry loop.
type ClientHooks struct {
	// OnPaired runs synchronously after the numeric peer is durable.
	OnPaired   func(transport.PeerInfo)
	Interfaces func(context.Context) ([]carside.NIC, error)
	Discover   func(context.Context, carside.DiscoverOptions) ([]carside.Gateway, error)
	Wait       func(context.Context, time.Duration) error
}

// DirectPair contains credentials for one explicit bootstrap dial.
type DirectPair struct{ Address, Code string }

// ClientConfig accepts exactly one pairing source. Zero durations use defaults;
// negative durations are configuration errors. NICAddr is never substituted.
type ClientConfig struct {
	CenterCode             string
	Center                 *settings.Center
	AppVersion             string
	StateDir               string
	AllowLinkTest          bool
	DirectPair             *DirectPair
	Invite                 string
	InviteFile             string
	Peer                   identity.Fingerprint
	NICName                string
	NICAddr                netip.Addr
	DiscoveryDestinations  []netip.AddrPort
	DiscoveryTimeout       time.Duration
	DiscoveryRetryInterval time.Duration
	NICCheckInterval       time.Duration
	RetryMin               time.Duration
	RetryMax               time.Duration
	Logger                 *slog.Logger
	Hooks                  ClientHooks
}

func (c *ClientConfig) defaults() error {
	if c.StateDir == "" {
		return fmt.Errorf("app: client state directory required")
	}
	choices := 0
	for _, set := range []bool{c.Invite != "", c.InviteFile != "", !c.Peer.IsZero(), c.DirectPair != nil, c.Center != nil || c.CenterCode != ""} {
		if set {
			choices++
		}
	}
	if choices != 1 {
		return fmt.Errorf("app: choose exactly one of invitation, invitation file, paired peer, direct pairing or center")
	}
	if c.CenterCode != "" && c.Center == nil {
		return centerError(centerflow.CenterUnconfigured)
	}
	if d := c.DirectPair; d != nil {
		addr, err := invite.ParseAddress(d.Address)
		if err != nil {
			return err
		}
		if len(d.Code) != 8 {
			return invite.ErrInvalidCode
		}
		for _, digit := range d.Code {
			if digit < '0' || digit > '9' {
				return invite.ErrInvalidCode
			}
		}
		c.DirectPair = &DirectPair{Address: addr, Code: d.Code}
	}
	if c.NICAddr.IsValid() && !unicastIPv4(c.NICAddr) {
		return fmt.Errorf("app: NIC address must be unicast IPv4")
	}
	for _, d := range []struct {
		value *time.Duration
		label string
		def   time.Duration
	}{
		{&c.DiscoveryTimeout, "discovery timeout", 3 * time.Second},
		{&c.DiscoveryRetryInterval, "discovery retry interval", time.Second},
		{&c.NICCheckInterval, "NIC check interval", time.Second},
		{&c.RetryMin, "minimum retry delay", 100 * time.Millisecond},
		{&c.RetryMax, "maximum retry delay", 5 * time.Second},
	} {
		if *d.value < 0 {
			return fmt.Errorf("app: %s must be positive", d.label)
		}
		if *d.value == 0 {
			*d.value = d.def
		}
	}
	if c.RetryMax < c.RetryMin {
		return fmt.Errorf("app: maximum retry delay is below minimum retry delay")
	}
	for _, destination := range c.DiscoveryDestinations {
		if !destination.IsValid() || !destination.Addr().Is4() || destination.Port() == 0 {
			return fmt.Errorf("app: discovery destination must be IPv4 with a nonzero port")
		}
	}
	c.DiscoveryDestinations = append([]netip.AddrPort(nil), c.DiscoveryDestinations...)
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Hooks.Interfaces == nil {
		c.Hooks.Interfaces = func(ctx context.Context) ([]carside.NIC, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return carside.Interfaces()
		}
	}
	if c.Hooks.Discover == nil {
		c.Hooks.Discover = carside.Discover
	}
	if c.Hooks.Wait == nil {
		c.Hooks.Wait = pause
	}
	return nil
}

func unicastIPv4(addr netip.Addr) bool {
	return addr.Is4() && (addr.IsGlobalUnicast() || addr.IsLinkLocalUnicast() || addr.IsLoopback())
}

func endpoint(addr string) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return fmt.Errorf("endpoint must be host:port")
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil || p == 0 {
		return fmt.Errorf("endpoint port must be in 1..65535")
	}
	if ip, err := netip.ParseAddr(host); err == nil && ip.IsUnspecified() {
		return fmt.Errorf("endpoint must name a reachable host, not an unspecified address")
	}
	return nil
}

func wrap(id *identity.Local) hubtransport.Wrap {
	return func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
		s, err := secure.WrapStream(ctx, conn, secure.Opts{Local: id, Expect: expect, IsClient: isClient, Path: path})
		if isClient && path.Kind == transport.PathRelay && errors.Is(err, identity.ErrFingerprintMismatch) {
			return nil, fmt.Errorf("%w: %w", PairingErrorKind("identity_mismatch"), err)
		}
		return s, err
	}
}
