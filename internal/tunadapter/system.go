package tunadapter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func systemBackend() backend {
	return backend{interfaces: net.Interfaces, addresses: func(i net.Interface) ([]net.Addr, error) { return i.Addrs() }, routes: systemRoutes, create: createNative, run: runCommand, os: runtime.GOOS, ready: nativeReadiness}
}
func plan(platform, name string, c Config) []operation {
	local := c.Local.Addr().String()
	mtu := fmt.Sprint(c.MTU)
	var ops []operation
	switch platform {
	case "linux":
		ops = append(ops, operation{add: []string{"ip", "link", "set", "dev", name, "mtu", mtu, "up"}}, operation{[]string{"ip", "address", "add", c.Local.String(), "dev", name, "noprefixroute"}, []string{"ip", "address", "del", c.Local.String(), "dev", name}})
		for _, t := range c.Targets {
			ops = append(ops, operation{[]string{"ip", "route", "add", t.String() + "/32", "dev", name, "src", local, "proto", "static"}, []string{"ip", "route", "del", t.String() + "/32", "dev", name, "src", local, "proto", "static"}})
		}
	case "darwin":
		ops = append(ops, operation{[]string{"ifconfig", name, "inet", local, local, "netmask", "255.255.255.255", "mtu", mtu, "up"}, []string{"ifconfig", name, "inet", local, "-alias"}})
		for _, t := range c.Targets {
			ops = append(ops, operation{[]string{"route", "-n", "add", "-host", t.String(), "-interface", name}, []string{"route", "-n", "delete", "-host", t.String(), "-interface", name}})
		}
	case "windows":
		ops = append(ops, operation{add: []string{"netsh", "interface", "ipv4", "set", "subinterface", name, "mtu=" + mtu, "store=active"}}, operation{[]string{"netsh", "interface", "ipv4", "add", "address", "name=" + name, "address=" + local, "mask=255.255.255.255", "store=active", "skipassource=false"}, []string{"netsh", "interface", "ipv4", "delete", "address", "name=" + name, "address=" + local, "store=active"}})
		for _, t := range c.Targets {
			ops = append(ops, operation{[]string{"netsh", "interface", "ipv4", "add", "route", "prefix=" + t.String() + "/32", "interface=" + name, "nexthop=0.0.0.0", "store=active"}, []string{"netsh", "interface", "ipv4", "delete", "route", "prefix=" + t.String() + "/32", "interface=" + name, "nexthop=0.0.0.0", "store=active"}})
		}
	}
	return ops
}

// commandOutput caps both captured bytes and execution time; no shell is involved.
func commandOutput(ctx context.Context, args []string) ([]byte, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("empty command")
	}
	path, err := toolPath(args[0])
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args[1:]...)
	cmd.WaitDelay = time.Second
	output := &limitedOutput{remaining: 1 << 20}
	cmd.Stdout = output
	cmd.Stderr = output
	if err = cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w (%s)", args[0], err, output.buffer.String())
	}
	return output.buffer.Bytes(), nil
}

type limitedOutput struct {
	buffer    bytes.Buffer
	remaining int
}

func (w *limitedOutput) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		return 0, fmt.Errorf("system command output exceeds 1 MiB")
	}
	n, err := w.buffer.Write(p)
	w.remaining -= n
	return n, err
}

var _ io.Writer = (*limitedOutput)(nil)

func runCommand(ctx context.Context, args []string) error {
	_, err := commandOutput(ctx, args)
	return err
}
func toolPath(name string) (string, error) {
	allowed := map[string]bool{"ip": true, "ifconfig": true, "route": true, "netstat": true, "netsh": true}
	if !allowed[name] {
		return "", fmt.Errorf("unsupported system tool %q", name)
	}
	if runtime.GOOS == "windows" {
		return windowsToolPath(name)
	}
	for _, dir := range []string{"/usr/sbin", "/sbin", "/usr/bin", "/bin"} {
		path := filepath.Join(dir, name)
		if _, err := exec.LookPath(path); err == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("system tool %s unavailable", name)
}
func systemRoutes(ctx context.Context) ([]routeEntry, error) {
	var args []string
	switch runtime.GOOS {
	case "linux":
		args = []string{"ip", "-j", "-4", "route", "show", "table", "all"}
	case "darwin":
		args = []string{"netstat", "-rn", "-f", "inet"}
	case "windows":
		args = []string{"route", "print", "-4"}
	default:
		return nil, fmt.Errorf("unsupported platform")
	}
	output, err := commandOutput(ctx, args)
	if err != nil {
		return nil, err
	}
	return parseRoutes(runtime.GOOS, output)
}
func parseRoutes(platform string, output []byte) ([]routeEntry, error) {
	var routes []routeEntry
	if platform == "linux" {
		var rows []struct {
			Dst string `json:"dst"`
		}
		if err := json.Unmarshal(output, &rows); err != nil {
			return nil, fmt.Errorf("decode routes: %w", err)
		}
		for _, r := range rows {
			if r.Dst == "default" || r.Dst == "" {
				continue
			}
			p, err := parseDestination(r.Dst)
			if err != nil {
				return nil, err
			}
			routes = append(routes, routeEntry{Prefix: p})
		}
		return routes, nil
	}
	for _, line := range strings.Split(string(output), "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if platform == "darwin" && (f[0] == "default" || f[0][0] >= '0' && f[0][0] <= '9') && len(f) < 4 {
			return nil, fmt.Errorf("incomplete Darwin route metadata: %q", line)
		}
		if len(f) < 2 {
			continue
		}
		if platform == "windows" {
			ip, err := netip.ParseAddr(f[0])
			if err != nil || !ip.Is4() {
				continue
			}
			mask, err := netip.ParseAddr(f[1])
			if err != nil || !mask.Is4() {
				return nil, fmt.Errorf("invalid route mask %q", f[1])
			}
			m := mask.As4()
			ones, bits := net.IPMask(m[:]).Size()
			if bits != 32 {
				return nil, fmt.Errorf("noncontiguous route mask")
			}
			routes = append(routes, routeEntry{Prefix: netip.PrefixFrom(ip, ones)})
			continue
		}
		if f[0] == "default" {
			continue
		}
		if f[0][0] < '0' || f[0][0] > '9' {
			continue
		}
		p, err := parseDestination(f[0])
		if err != nil {
			return nil, err
		}
		routes = append(routes, routeEntry{Prefix: p, Gateway: f[1], Flags: f[2], Netif: f[3]})
	}
	return routes, nil
}
func parseDestination(s string) (netip.Prefix, error) {
	address, bits, hasBits := strings.Cut(s, "/")
	parts := strings.Split(address, ".")
	if len(parts) > 4 {
		return netip.Prefix{}, fmt.Errorf("invalid route %q", s)
	}
	if !hasBits {
		bits = fmt.Sprint(len(parts) * 8)
	}
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	p, err := netip.ParsePrefix(strings.Join(parts, ".") + "/" + bits)
	if err != nil {
		return p, fmt.Errorf("invalid route %q: %w", s, err)
	}
	return p, nil
}
