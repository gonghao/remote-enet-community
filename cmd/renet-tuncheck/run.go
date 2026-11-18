package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"remote-enet/internal/l3"
	"remote-enet/internal/tunadapter"
)

func execute(parent context.Context, cfg config, deps dependencies, initial report) (r report) {
	r = initial
	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()
	var device adapter
	var stack *l3.Stack
	var remote *fixture
	var sshProbe *bannerProbe
	checksPassed := false
	defer func() {
		if remote != nil {
			remote.beginClose()
		}
		cleanup := func(name string, err error) {
			r.record(name, err)
			if err != nil {
				r.CleanupErrors = append(r.CleanupErrors, name+": "+err.Error())
			}
		}
		if sshProbe != nil {
			cleanup("close_ssh_probe", sshProbe.Close())
		}
		if stack != nil {
			cleanup("close_l3", stack.Close())
			r.L3 = stack.Snapshot()
		}
		if device != nil {
			cleanup("close_adapter", device.Close())
		}
		if remote != nil {
			cleanup("close_fixture", remote.Close())
			r.Flows = remote.snapshot()
		}
		if device != nil {
			// Cleanup verification has its own short budget even after a signal or timeout.
			afterCtx, stop := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
			banner, err := readBanner(afterCtx, deps.sshAddr, nil)
			stop()
			r.SSH.After = banner
			r.record("host_ssh_after_cleanup", err)
		}
		if checksPassed {
			r.record("flow_headers", verifyFlows(r.Flows, cfg))
			var counterErr error
			if r.L3.Active != 0 || r.L3.Accepted < 2 {
				counterErr = fmt.Errorf("expected at least two accepted flows and no active flows: %+v", r.L3)
			}
			r.record("l3_final_counts", counterErr)
			var bannerErr error
			if r.SSH.Before != r.SSH.During || r.SSH.Before != r.SSH.After || r.SSH.Before != r.SSH.Tunnel {
				bannerErr = fmt.Errorf("host and tunnel SSH banners differ")
			}
			r.record("ssh_banner_consistency", bannerErr)
		}
		r.record("operation_context", ctx.Err())
		r.Success = checksPassed && len(r.CleanupErrors) == 0
		for _, item := range r.Stages {
			if !item.Passed {
				r.Success = false
			}
		}
	}()
	banner, err := readBanner(ctx, deps.sshAddr, nil)
	r.SSH.Before = banner
	if !r.record("host_ssh_before_native", err) {
		return r
	}
	remote, err = startFixture(ctx, cfg.local, cfg.target, deps.sshAddr)
	if !r.record("start_mtls_session_fixture", err) {
		return r
	}
	device, err = deps.open(ctx, tunadapter.Config{Name: cfg.name, Local: netip.PrefixFrom(cfg.local, 32), Targets: []netip.Addr{cfg.target}, MTU: 1500})
	if device != nil {
		r.Interface = device.Name()
	}
	if !r.record("open_native_adapter", err) {
		return r
	}
	stack, err = l3.Start(ctx, device, l3.Options{Targets: []netip.Addr{cfg.target}, Opener: remote.operator, MaxConnections: 4})
	if !r.record("start_l3", err) {
		return r
	}
	r.SSH.During, err = readBanner(ctx, deps.sshAddr, nil)
	if !r.record("host_ssh_during_native", err) {
		return r
	}
	source := &net.TCPAddr{IP: net.IP(cfg.local.AsSlice())}
	r.SSH.Tunnel, sshProbe, err = readBannerHeld(ctx, netip.AddrPortFrom(cfg.target, 22).String(), source)
	if !r.record("tunnel_ssh_banner_only", err) {
		return r
	}
	err = checkHTTP(ctx, netip.AddrPortFrom(cfg.target, 8080).String(), source)
	r.HTTPVerified = err == nil
	if !r.record("tunnel_http_get", err) {
		return r
	}
	checksPassed = true
	return r
}

func verifyFlows(flows []flow, cfg config) error {
	wanted := map[string]bool{netip.AddrPortFrom(cfg.target, 22).String(): false, netip.AddrPortFrom(cfg.target, 8080).String(): false}
	for _, f := range flows {
		if f.Error != "" {
			return fmt.Errorf("fixture flow %s from %s failed: %s", f.Target, f.Origin, f.Error)
		}
		if _, ok := wanted[f.Target]; !ok || f.Kind != "tcp" || !f.OriginMatchesLocal {
			return fmt.Errorf("unexpected fixture flow: %s %s from %s", f.Kind, f.Target, f.Origin)
		}
		origin, err := netip.ParseAddrPort(f.Origin)
		if err != nil || origin.Addr() != cfg.local || origin.Port() == 0 {
			return fmt.Errorf("unexpected origin %q", f.Origin)
		}
		wanted[f.Target] = true
	}
	for dst, seen := range wanted {
		if !seen {
			return fmt.Errorf("fixture did not observe %s", dst)
		}
	}
	return nil
}

type bannerProbe struct {
	net.Conn
	once     sync.Once
	stop     func() bool
	closeErr error
}

func (p *bannerProbe) closeConn() error {
	p.once.Do(func() { p.closeErr = p.Conn.Close() })
	return p.closeErr
}

func (p *bannerProbe) Close() error {
	p.stop()
	return p.closeConn()
}

func readBanner(ctx context.Context, address string, local *net.TCPAddr) (string, error) {
	banner, probe, err := readBannerHeld(ctx, address, local)
	if probe != nil {
		defer probe.Close()
	}
	return banner, err
}

func readBannerHeld(ctx context.Context, address string, local *net.TCPAddr) (string, *bannerProbe, error) {
	dialer := net.Dialer{Timeout: 3 * time.Second, LocalAddr: local}
	conn, err := dialer.DialContext(ctx, "tcp4", address)
	if err != nil {
		return "", nil, fmt.Errorf("SSH banner dial %s: %w", address, err)
	}
	probe := &bannerProbe{Conn: conn}
	probe.stop = context.AfterFunc(ctx, func() { _ = probe.closeConn() })
	deadline := time.Now().Add(3 * time.Second)
	if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
		deadline = end
	}
	_ = conn.SetDeadline(deadline)
	banner, err := parseBanner(conn, address)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = probe.Close()
		return "", nil, err
	}
	// Keep the banner-only connection alive through validation, until marked cleanup.
	heldDeadline, _ := ctx.Deadline()
	_ = conn.SetDeadline(heldDeadline)
	return banner, probe, nil
}

func parseBanner(input io.Reader, address string) (string, error) {
	reader := bufio.NewReader(io.LimitReader(input, 8192))
	for range 50 {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", fmt.Errorf("SSH banner read %s: %w", address, err)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if strings.HasPrefix(line, "SSH-") {
			if len(line) > 253 || !(strings.HasPrefix(line, "SSH-2.0-") || strings.HasPrefix(line, "SSH-1.99-")) {
				return "", fmt.Errorf("invalid SSH identification")
			}
			for _, c := range line {
				if c < 32 || c > 126 {
					return "", fmt.Errorf("invalid SSH identification characters")
				}
			}
			return line, nil
		}
	}
	return "", fmt.Errorf("SSH identification not found within banner budget")
}

func checkHTTP(ctx context.Context, address string, local *net.TCPAddr) error {
	dialer := net.Dialer{Timeout: 3 * time.Second, LocalAddr: local}
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: dialer.DialContext, ResponseHeaderTimeout: 3 * time.Second}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("fixture redirects are forbidden") }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/", nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("HTTP fixture GET: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(len(fixtureBody)+1)))
	if err != nil {
		return fmt.Errorf("HTTP fixture body: %w", err)
	}
	if resp.StatusCode != http.StatusOK || string(data) != fixtureBody {
		return fmt.Errorf("HTTP fixture response mismatch: status=%d body=%q", resp.StatusCode, data)
	}
	return nil
}
