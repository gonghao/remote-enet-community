// renet-tuncheck is an explicit, temporary native TUN acceptance harness.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"remote-enet/internal/l3"
	"remote-enet/internal/packetio"
	"remote-enet/internal/reportfile"
	"remote-enet/internal/tunadapter"
)

var version = "dev"

type config struct {
	run                                     bool
	localText, targetText, name, reportPath string
	timeout                                 time.Duration
	local, target                           netip.Addr
}

type adapter interface {
	packetio.Device
	Name() string
}
type dependencies struct {
	open      func(context.Context, tunadapter.Config) (adapter, error)
	hostAddrs func() ([]net.Addr, error)
	sshAddr   string
}

func nativeDependencies() dependencies {
	return dependencies{
		open: func(ctx context.Context, cfg tunadapter.Config) (adapter, error) {
			native, err := tunadapter.Open(ctx, cfg)
			if native == nil {
				return nil, err
			}
			return native, err
		},
		hostAddrs: net.InterfaceAddrs,
		sshAddr:   "127.0.0.1:22",
	}
}

type stage struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Error  string `json:"error,omitempty"`
}
type sshResults struct {
	Before string `json:"host_before,omitempty"`
	During string `json:"host_during,omitempty"`
	After  string `json:"host_after,omitempty"`
	Tunnel string `json:"tunnel_banner,omitempty"`
}
type report struct {
	Platform      string      `json:"platform"`
	Version       string      `json:"version"`
	Mode          string      `json:"mode"`
	Scope         string      `json:"scope"`
	Interface     string      `json:"interface,omitempty"`
	Local         string      `json:"local"`
	Target        string      `json:"target"`
	Success       bool        `json:"success"`
	Stages        []stage     `json:"stages"`
	Flows         []flow      `json:"flows"`
	L3            l3.Snapshot `json:"l3"`
	SSH           sshResults  `json:"ssh"`
	HTTPVerified  bool        `json:"http_verified"`
	CleanupErrors []string    `json:"cleanup_errors"`
	Started       time.Time   `json:"started_at"`
	Finished      time.Time   `json:"finished_at"`
}

func (r *report) record(name string, err error) bool {
	item := stage{Name: name, Passed: err == nil}
	if err != nil {
		item.Error = err.Error()
	}
	r.Stages = append(r.Stages, item)
	return err == nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := runCLI(ctx, os.Args[1:], os.Stdout, os.Stderr, nativeDependencies())
	stop()
	os.Exit(code)
}

func runCLI(ctx context.Context, args []string, out, errOut io.Writer, deps dependencies) int {
	cfg := config{}
	fs := flag.NewFlagSet("renet-tuncheck", flag.ContinueOnError)
	fs.SetOutput(errOut)
	fs.BoolVar(&cfg.run, "run", false, "create a temporary native interface and host route; otherwise dry-run")
	fs.StringVar(&cfg.localText, "local", "198.18.8.1", "new interface IPv4 host address (benchmark range only)")
	fs.StringVar(&cfg.targetText, "target", "198.18.8.2", "one simulated IPv4 host destination (benchmark range only)")
	fs.StringVar(&cfg.name, "name", "", "new interface name; empty selects the platform default (automatic utun on macOS)")
	fs.DurationVar(&cfg.timeout, "timeout", 30*time.Second, "total operation budget, from 1s through 120s")
	fs.StringVar(&cfg.reportPath, "report", "", "write JSON report here; default is stdout")
	showVersion := fs.Bool("version", false, "print version without opening an interface")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		fmt.Fprintln(out, version)
		return 0
	}
	r := report{Platform: runtime.GOOS + "/" + runtime.GOARCH, Version: version, Mode: "dry-run", Scope: "local fixture only; SSH banner, no authentication; not a real MGU acceptance test", Local: cfg.localText, Target: cfg.targetText, Started: time.Now(), Stages: []stage{}, Flows: []flow{}, CleanupErrors: []string{}}
	if cfg.run {
		r.Mode = "native"
	}
	err := validate(&cfg, fs.Args(), deps.hostAddrs)
	if r.record("validate_before_native", err) {
		r.Local = cfg.local.String()
		r.Target = cfg.target.String()
		if cfg.run {
			r = execute(ctx, cfg, deps, r)
		}
	}
	r.Finished = time.Now()
	if err := writeReport(out, cfg.reportPath, r); err != nil {
		fmt.Fprintln(errOut, err)
		return 1
	}
	if err != nil || cfg.run && !r.Success {
		return 1
	}
	return 0
}

func validate(cfg *config, args []string, hostAddrs func() ([]net.Addr, error)) error {
	if len(args) > 0 {
		return fmt.Errorf("unexpected positional arguments: %v", args)
	}
	if cfg.timeout < time.Second || cfg.timeout > 120*time.Second {
		return fmt.Errorf("timeout must be between 1s and 120s")
	}
	var err error
	cfg.local, err = parseHost(cfg.localText)
	if err != nil {
		return fmt.Errorf("local: %w", err)
	}
	cfg.target, err = parseHost(cfg.targetText)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if cfg.local == cfg.target {
		return fmt.Errorf("local and target must differ")
	}
	addresses, err := hostAddrs()
	if err != nil {
		return fmt.Errorf("inspect host addresses: %w", err)
	}
	for _, raw := range addresses {
		prefix, err := netip.ParsePrefix(raw.String())
		if err != nil {
			continue
		}
		addr := prefix.Addr().Unmap()
		if addr == cfg.local || addr == cfg.target {
			return fmt.Errorf("address %s is already assigned to the host", addr)
		}
	}
	return nil
}

func parseHost(text string) (netip.Addr, error) {
	addr, err := netip.ParseAddr(text)
	if err != nil {
		prefix, prefixErr := netip.ParsePrefix(text)
		if prefixErr != nil || prefix.Bits() != 32 {
			return netip.Addr{}, fmt.Errorf("expected one IPv4 host address or /32")
		}
		addr = prefix.Addr()
	}
	if !addr.Is4() || !netip.MustParsePrefix("198.18.0.0/15").Contains(addr) {
		return netip.Addr{}, fmt.Errorf("manual fixture addresses must be IPv4 hosts in 198.18.0.0/15")
	}
	return addr, nil
}

func writeReport(out io.Writer, path string, r report) error {
	if path == "" {
		return json.NewEncoder(out).Encode(r)
	}
	if err := reportfile.Write(path, ".tuncheck-", func(w io.Writer) error {
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(r); err != nil {
			return fmt.Errorf("encode report: %w", err)
		}
		return nil
	}); err != nil {
		return fmt.Errorf("write report: %w", err)
	}
	return nil
}
