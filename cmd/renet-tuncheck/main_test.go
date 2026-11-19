package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/packetio"
	"remote-enet/internal/proto"
	"remote-enet/internal/tunadapter"
)

const testBanner = "SSH-2.0-renet-test"

func bannerServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			workers.Add(1)
			go func() {
				defer workers.Done()
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(2 * time.Second))
				_, _ = io.WriteString(c, "fixture preamble\r\n"+testBanner+"\r\n")
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	t.Cleanup(func() { _ = ln.Close(); <-done; workers.Wait() })
	return ln.Addr().String()
}

func testDependencies(t *testing.T) dependencies {
	t.Helper()
	return dependencies{open: func(context.Context, tunadapter.Config) (adapter, error) {
		t.Error("native adapter factory called")
		return nil, errors.New("unexpected native access")
	}, hostAddrs: func() ([]net.Addr, error) { return nil, nil }, sshAddr: "127.0.0.1:1"}
}

func TestCLIHasNoNativeSideEffectsByDefault(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"--version"}, {"--timeout", "121s"}, {"--run", "--target", "0.0.0.0/0"}, {"--run", "--local", "127.0.0.1"}, {"--run", "--target", "198.18.8.0/24"}, {"--run", "--target", "198.18.8.1"}, {"--run", "--target", "::ffff:198.18.8.2"}, {"--run", "extra"}} {
		t.Run(fmt.Sprint(args), func(t *testing.T) {
			var out, stderr bytes.Buffer
			deps := testDependencies(t)
			code := runCLI(context.Background(), args, &out, &stderr, deps)
			if len(args) == 0 {
				if code != 0 {
					t.Fatal(stderr.String())
				}
				var r report
				if err := json.Unmarshal(out.Bytes(), &r); err != nil {
					t.Fatal(err)
				}
				if r.Mode != "dry-run" || r.Success || len(r.Stages) != 1 {
					t.Fatal(r)
				}
			}
		})
	}
}

func TestConfiguredHostAddressRejectedBeforeNative(t *testing.T) {
	deps := testDependencies(t)
	deps.hostAddrs = func() ([]net.Addr, error) {
		return []net.Addr{&net.IPNet{IP: net.ParseIP("198.18.8.2"), Mask: net.CIDRMask(32, 32)}}, nil
	}
	var out, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"--run"}, &out, &stderr, deps); code != 1 {
		t.Fatal(code)
	}
	var r report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Stages) != 1 || !strings.Contains(r.Stages[0].Error, "already assigned") {
		t.Fatal(r)
	}
}

func TestSSHPreflightFailureAndReportFile(t *testing.T) {
	deps := testDependencies(t)
	// Reserve then close a loopback listener instead of assuming a service is absent.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	deps.sshAddr = ln.Addr().String()
	_ = ln.Close()
	path := filepath.Join(t.TempDir(), "report.json")
	var out, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"--run", "--report", path}, &out, &stderr, deps); code != 1 {
		t.Fatal(code)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Success || r.Interface != "" || r.Stages[1].Name != "host_ssh_before_native" || r.Stages[1].Passed {
		t.Fatal(r)
	}
}

func TestEncryptedSessionFixtureBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	local := netip.MustParseAddr("198.18.8.1")
	target := netip.MustParseAddr("198.18.8.2")
	f, err := startFixture(ctx, local, target, bannerServer(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
	if f.operator.Peer().IsZero() || f.client.Peer().IsZero() || f.operator.Peer() == f.client.Peer() {
		t.Fatal("fixture identities were not distinct")
	}
	for _, port := range []uint16{22, 8080} {
		s, err := f.operator.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: netip.AddrPortFrom(target, port).String(), Origin: netip.AddrPortFrom(local, 40000+port).String()})
		if err != nil {
			t.Fatal(err)
		}
		stop := context.AfterFunc(ctx, func() { _ = s.Close() })
		if port == 22 {
			reader := bufio.NewReader(s)
			_, _ = reader.ReadString('\n')
			line, err := reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != testBanner {
				t.Fatalf("banner %q %v", line, err)
			}
		} else {
			_, err := io.WriteString(s, "GET / HTTP/1.1\r\nHost: fixture\r\nConnection: close\r\n\r\n")
			if err != nil {
				t.Fatal(err)
			}
			resp, err := http.ReadResponse(bufio.NewReader(s), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil || string(body) != fixtureBody {
				t.Fatalf("HTTP body %q %v", body, err)
			}
		}
		_ = s.CloseWrite()
		_ = s.Close()
		stop()
	}
	for _, h := range []proto.OpenTunnel{
		{Kind: "reverse", Target: "198.18.8.2:22", Origin: "198.18.8.1:40001"},
		{Kind: "tcp", Target: "198.18.8.2:23", Origin: "198.18.8.1:40001"},
		{Kind: "tcp", Target: "203.0.113.1:22", Origin: "198.18.8.1:40001"},
		{Kind: "tcp", Target: "127.0.0.1:22", Origin: "198.18.8.1:40001"},
		{Kind: "tcp", Target: "198.18.8.2:22", Origin: "198.18.8.3:40001"},
		{Kind: "tcp", Target: "198.18.8.2:22", Origin: "198.18.8.1:0"},
	} {
		if _, err := f.destination(h); err == nil {
			t.Fatalf("accepted forbidden header %+v", h)
		}
	}
	// Exercise a rejected target through the authenticated session, beyond the helper.
	s, err := f.operator.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: "203.0.113.1:22", Origin: "198.18.8.1:40001"})
	if err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = s.Close() })
	defer stop()
	defer s.Close()
	data, err := io.ReadAll(s)
	if len(data) != 0 || err != nil {
		t.Fatalf("denied stream %q %v", data, err)
	}
}

type errorAdapter struct {
	packetio.Device
	closes atomic.Int32
}

func (*errorAdapter) Name() string { return "memory-fixture" }
func (d *errorAdapter) Close() error {
	d.closes.Add(1)
	return errors.Join(d.Device.Close(), errors.New("simulated cleanup failure"))
}

func TestOpenFailureStillCleansAndReports(t *testing.T) {
	deps := testDependencies(t)
	deps.sshAddr = bannerServer(t)
	a, _, err := packetio.NewPair(1500, 4)
	if err != nil {
		t.Fatal(err)
	}
	device := &errorAdapter{Device: a}
	deps.open = func(context.Context, tunadapter.Config) (adapter, error) {
		return device, errors.New("simulated native configuration failure")
	}
	var out, stderr bytes.Buffer
	if code := runCLI(context.Background(), []string{"--run", "--timeout", "5s"}, &out, &stderr, deps); code != 1 {
		t.Fatal(code)
	}
	var r report
	if err := json.Unmarshal(out.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if r.Success || device.closes.Load() != 1 || len(r.CleanupErrors) != 1 || r.SSH.After != testBanner || r.SSH.Before != testBanner {
		t.Fatalf("bad cleanup report %+v", r)
	}
	if r.Interface != "memory-fixture" || !strings.Contains(r.CleanupErrors[0], "simulated cleanup failure") {
		t.Fatal(r)
	}
}

func TestProbeLimitsAndFixtureHTTP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := bannerServer(t)
	if banner, err := readBanner(ctx, addr, nil); err != nil || banner != testBanner {
		t.Fatalf("banner %q %v", banner, err)
	}
	f, err := startFixture(ctx, netip.MustParseAddr("198.18.8.1"), netip.MustParseAddr("198.18.8.2"), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	if err := checkHTTP(ctx, f.httpAddr, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := startFixture(ctx, f.local, f.target, "203.0.113.1:22"); err == nil {
		t.Fatal("nonloopback fixture endpoint accepted")
	}
}

func TestFixtureCancellationJoinsActiveStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	f, err := startFixture(ctx, netip.MustParseAddr("198.18.8.1"), netip.MustParseAddr("198.18.8.2"), bannerServer(t))
	if err != nil {
		t.Fatal(err)
	}
	s, err := f.operator.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: "198.18.8.2:22", Origin: "198.18.8.1:40000"})
	if err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	defer s.Close()
	reader := bufio.NewReader(s)
	_, _ = reader.ReadString('\n')
	if _, err := reader.ReadString('\n'); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	f.beginClose()
	cancel()
	done := make(chan error, 1)
	go func() { done <- f.Close() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("fixture workers remained after cancellation")
	}
	if _, err := s.Read(make([]byte, 1)); err == nil {
		t.Fatal("canceled stream remained readable")
	}
	if len(f.snapshot()) != 1 {
		t.Fatal("fixture handler was not joined")
	}
	if got := f.snapshot()[0].Error; got != "" {
		t.Fatalf("cleanup became a business failure: %s", got)
	}
}

func TestVerifyFlowsRejectsAdditionalFailedFlow(t *testing.T) {
	cfg := config{local: netip.MustParseAddr("198.18.8.1"), target: netip.MustParseAddr("198.18.8.2")}
	flows := []flow{
		{Kind: "tcp", Target: "198.18.8.2:22", Origin: "198.18.8.1:40000", OriginMatchesLocal: true},
		{Kind: "tcp", Target: "198.18.8.2:8080", Origin: "198.18.8.1:40001", OriginMatchesLocal: true},
	}
	if err := verifyFlows(flows, cfg); err != nil {
		t.Fatal(err)
	}
	flows = append(flows, flow{Kind: "tcp", Target: "198.18.8.2:22", Origin: "198.18.8.1:40002", OriginMatchesLocal: true, Error: "fixture loopback dial failed"})
	if err := verifyFlows(flows, cfg); err == nil || !strings.Contains(err.Error(), "fixture loopback dial failed") {
		t.Fatalf("failed flow was accepted: %v", err)
	}
}

func TestClosingPreservesDestinationAndDialFailures(t *testing.T) {
	f := &fixture{local: netip.MustParseAddr("198.18.8.1"), target: netip.MustParseAddr("198.18.8.2")}
	_, destinationErr := f.destination(proto.OpenTunnel{Kind: "tcp", Target: "198.18.8.2:23", Origin: "198.18.8.1:40000"})
	if destinationErr == nil {
		t.Fatal("invalid destination accepted")
	}
	// Both errors have been decided before cleanup wins the report lock.
	f.beginClose()
	f.recordFlow(flow{Error: destinationErr.Error()}, context.Canceled)
	f.recordFlow(flow{Error: "fixture loopback dial: connection refused"}, context.Canceled)
	f.recordFlow(flow{}, context.Canceled)
	got := f.snapshot()
	if len(got) != 3 || got[0].Error != destinationErr.Error() || got[1].Error != "fixture loopback dial: connection refused" || got[2].Error != "" {
		t.Fatalf("incorrect cleanup classification: %+v", got)
	}
	// A pipe error outside cleanup remains a failure.
	running := &fixture{}
	running.recordFlow(flow{}, io.ErrUnexpectedEOF)
	if running.snapshot()[0].Error != io.ErrUnexpectedEOF.Error() {
		t.Fatal("pipe failure was lost")
	}
}

func TestHeldBannerProbeLifetime(t *testing.T) {
	for _, cancelClose := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelClose), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			ln, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			received := make(chan string, 1)
			eof := make(chan error, 1)
			serverDone := make(chan struct{})
			go func() {
				defer close(serverDone)
				conn, err := ln.Accept()
				if err != nil {
					eof <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
				if _, err := io.WriteString(conn, testBanner+"\r\n"); err != nil {
					eof <- err
					return
				}
				data := make([]byte, len("held-probe"))
				if _, err := io.ReadFull(conn, data); err != nil {
					eof <- err
					return
				}
				received <- string(data)
				_, err = conn.Read(make([]byte, 1))
				eof <- err
			}()
			defer func() { _ = ln.Close(); <-serverDone }()
			banner, probe, err := readBannerHeld(ctx, ln.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer probe.Close()
			if banner != testBanner {
				t.Fatal(banner)
			}
			// Test bytes are sent only to this synthetic loopback server, never sshd.
			if _, err := probe.Write([]byte("held-probe")); err != nil {
				t.Fatalf("probe closed after banner: %v", err)
			}
			select {
			case got := <-received:
				if got != "held-probe" {
					t.Fatal(got)
				}
			case <-ctx.Done():
				t.Fatal("server did not receive held connection bytes")
			}
			if cancelClose {
				cancel()
			} else {
				if err := probe.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-eof:
				if !errors.Is(err, io.EOF) {
					t.Fatalf("server did not see EOF: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("probe close did not unblock server")
			}
			if err := probe.Close(); err != nil {
				t.Fatalf("repeated Close: %v", err)
			}
		})
	}
}

func TestWriteReportReplacesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	if err := os.WriteFile(path, []byte("old report"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	old, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := writeReport(io.Discard, path, report{}); err != nil {
		t.Fatal(err)
	}
	current, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && os.SameFile(old, current) {
		t.Fatal("report was overwritten in place")
	}
	// Windows uses directory ACLs for file access control.
	if runtime.GOOS != "windows" && current.Mode().Perm() != 0o600 {
		t.Fatalf("report mode = %o, want 600", current.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(data) {
		t.Fatalf("invalid report: %q", data)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("report directory: %v, %v", entries, err)
	}
}
