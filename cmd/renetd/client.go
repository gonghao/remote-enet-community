package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/identity"
)

func runClient(ctx context.Context, args []string, stderr io.Writer, logger *slog.Logger) error {
	fs := flags("client", stderr)
	stateDir := fs.String("state-dir", "", "client state directory (required)")
	inviteCode := fs.String("invite", "", "first-pairing invitation (prefer --invite-file to avoid shell history)")
	inviteFile := fs.String("invite-file", "", "file containing a first-pairing invitation")
	peerID := fs.String("peer", "", "previously paired operator fingerprint")
	nicName := fs.String("nic", "", "vehicle interface name; auto-select a unique link-local interface when omitted")
	nicAddr := fs.String("nic-addr", "", "explicit IPv4 source address on the vehicle interface")
	discoveryTarget := fs.String("discovery-target", "", "optional IPv4:port unicast discovery destination")
	discoveryTimeout := fs.Duration("discovery-timeout", 3*time.Second, "vehicle discovery timeout")
	discoveryRetry := fs.Duration("discovery-retry", time.Second, "retry interval while waiting for a vehicle")
	nicCheck := fs.Duration("nic-check", time.Second, "vehicle interface check interval")
	opts := addRuntimeFlags(fs)
	if err := parsed(fs, args, stateDir); err != nil {
		return err
	}
	if err := opts.validate(); err != nil {
		return err
	}
	choices := 0
	for _, value := range []string{*inviteCode, *inviteFile, *peerID} {
		if value != "" {
			choices++
		}
	}
	if choices != 1 {
		return fmt.Errorf("choose exactly one of --invite-file, --invite, or --peer")
	}
	if *discoveryTimeout <= 0 || *discoveryRetry <= 0 || *nicCheck <= 0 {
		return fmt.Errorf("--discovery-timeout, --discovery-retry and --nic-check must be positive")
	}
	var selectedAddr netip.Addr
	var err error
	if *nicAddr != "" {
		selectedAddr, err = ipv4(*nicAddr, "--nic-addr")
		if err != nil {
			return err
		}
	}
	var destinations []netip.AddrPort
	if *discoveryTarget != "" {
		target, err := netip.ParseAddrPort(*discoveryTarget)
		if err != nil || !target.Addr().Is4() || target.Port() == 0 {
			return fmt.Errorf("--discovery-target must be an IPv4 address and a nonzero port")
		}
		destinations = []netip.AddrPort{target}
	}
	var pairedPeer identity.Fingerprint
	if *peerID != "" {
		pairedPeer, err = identity.ParseFingerprint(*peerID)
		if err != nil || pairedPeer.IsZero() {
			return fmt.Errorf("--peer must be a nonzero device fingerprint")
		}
	}
	r := app.New()
	return runRuntime(ctx, r, func(ctx context.Context) error {
		return r.RunClient(ctx, app.ClientConfig{
			StateDir: *stateDir, Invite: *inviteCode, InviteFile: *inviteFile, Peer: pairedPeer,
			NICName: *nicName, NICAddr: selectedAddr, DiscoveryDestinations: destinations,
			DiscoveryTimeout: *discoveryTimeout, DiscoveryRetryInterval: *discoveryRetry,
			NICCheckInterval: *nicCheck, Logger: logger, AllowLinkTest: opts.allowLinkTest,
		})
	}, *opts, logger)
}
