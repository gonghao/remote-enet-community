package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
)

func flags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parsed(fs *flag.FlagSet, args []string, stateDir *string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments")
	}
	if *stateDir == "" {
		return fmt.Errorf("--state-dir is required; use a separate directory for each role")
	}
	return nil
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

func ipv4(value, label string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(value)
	if err != nil || !addr.Is4() || addr.IsUnspecified() || addr.IsMulticast() {
		return netip.Addr{}, fmt.Errorf("%s must be a unicast IPv4 address", label)
	}
	return addr, nil
}

func port(value int, label string, zero bool) error {
	minimum := 1
	if zero {
		minimum = 0
	}
	if value < minimum || value > 65535 {
		return fmt.Errorf("%s must be in %d..65535", label, minimum)
	}
	return nil
}
