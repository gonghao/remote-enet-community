package checkup

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/netpriv"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunadapter"
)

type closerFunc func() error

func (c closerFunc) Close() error { return c() }

func fakeBackend(t *testing.T) backend {
	t.Helper()
	return backend{
		interfaces: func() ([]carside.NIC, error) {
			return []carside.NIC{{Name: "vehicle", Address: netip.MustParseAddr("169.254.5.1"), Prefix: netip.MustParsePrefix("169.254.5.1/16")}}, nil
		},
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
		},
		dial: func(context.Context, string) (io.Closer, error) { return closerFunc(func() error { return nil }), nil },
		port: func(context.Context, string, string) error { return nil },
		routes: func(context.Context, []netip.Addr) (netip.Prefix, error) {
			return netip.MustParsePrefix("192.0.2.1/32"), nil
		},
	}
}

func operatorInput() Input {
	cfg := settings.Default()
	cfg.Role = settings.RoleOperator
	cfg.Operator.Bind = "169.254.5.1"
	cfg.Operator.Endpoint = "fixture.invalid:47000"
	return Input{Config: cfg, Permission: netpriv.Status{Platform: runtime.GOOS, Elevated: true}}
}

func finding(t *testing.T, r Report, id string, index int) Finding {
	t.Helper()
	for _, f := range r.Findings {
		if f.ID == id && f.Index == index {
			return f
		}
	}
	t.Fatalf("missing finding %s/%d: %+v", id, index, r)
	return Finding{}
}

func TestClientSelectionAndBoundedCandidates(t *testing.T) {
	b := fakeBackend(t)
	in := Input{Config: settings.Default(), PeerCode: "renet1:secret"}
	r := run(t.Context(), in, b)
	if !r.Complete || finding(t, r, "vehicle_adapter", 0).Code != "adapter_selected" || finding(t, r, "peer_configuration", 0).Code != "pairing_missing" {
		t.Fatalf("unexpected client checks: %+v", r)
	}
	for i := range 7 {
		in.Endpoints = append(in.Endpoints, transport.Endpoint{Kind: "hub", Addr: "fixture.invalid:47000", Hub: identity.Fingerprint{byte(i + 1)}})
	}
	dials, lookups, closes := 0, 0, 0
	b.lookup = func(context.Context, string) ([]netip.Addr, error) {
		lookups++
		return []netip.Addr{netip.MustParseAddr("::1")}, nil
	}
	b.dial = func(context.Context, string) (io.Closer, error) {
		dials++
		return closerFunc(func() error { closes++; return nil }), nil
	}
	r = run(t.Context(), in, b)
	if dials != 4 || lookups != 4 || closes != 4 || finding(t, r, "hub_candidates", 0).Code != "endpoint_limit" {
		t.Fatalf("candidate budget/cleanup failed: %d/%d/%d %+v", dials, lookups, closes, r)
	}
	for _, tc := range []struct {
		name, address, code string
		nics                []carside.NIC
	}{
		{"", "", "adapter_ambiguous", []carside.NIC{{Name: "a", Address: netip.MustParseAddr("169.254.5.1"), Prefix: netip.MustParsePrefix("169.254.0.0/16")}, {Name: "b", Address: netip.MustParseAddr("169.254.6.1"), Prefix: netip.MustParsePrefix("169.254.0.0/16")}}},
		{"saved-missing", "", "adapter_unavailable", nil},
		{"vehicle", "169.254.5.2", "adapter_unavailable", []carside.NIC{{Name: "vehicle", Address: netip.MustParseAddr("169.254.5.1")}}},
	} {
		b.interfaces = func() ([]carside.NIC, error) { return tc.nics, nil }
		in.Config.Client.NICName, in.Config.Client.NICAddr = tc.name, tc.address
		r = run(t.Context(), in, b)
		if got := finding(t, r, "vehicle_adapter", 0).Code; got != tc.code {
			t.Fatalf("selection %q/%q: %s", tc.name, tc.address, got)
		}
	}
}

func TestOperatorIdleProbesAndRunningIsolation(t *testing.T) {
	b, in := fakeBackend(t), operatorInput()
	var ports []string
	b.port = func(_ context.Context, network, address string) error {
		ports = append(ports, network+" "+address)
		return syscall.EADDRINUSE
	}
	b.dial = func(context.Context, string) (io.Closer, error) {
		t.Fatal("operator must not dial its public endpoint")
		return nil, nil
	}
	r := run(t.Context(), in, b)
	if len(ports) != 4 || finding(t, r, "discovery_port", 0).Code != "port_in_use" || !strings.HasPrefix(ports[3], "udp4 ") || finding(t, r, "public_endpoint", 0).Code != "remote_check_required" {
		t.Fatalf("idle probe mismatch: %v %+v", ports, r)
	}
	ports = nil
	in.Running, in.Runtime.Phase = true, app.PhaseConnected
	r = run(t.Context(), in, b)
	if len(ports) != 0 || finding(t, r, "hub_port", 0).Status != Skipped || finding(t, r, "diagnostic_port", 0).Code != "runtime_active" || finding(t, r, "peer_session", 0).Code != "session_connected" {
		t.Fatalf("active session was probed: %v %+v", ports, r)
	}
	in.Config.Operator.Bind = "192.0.2.99"
	in.Running = false
	r = run(t.Context(), in, b)
	if len(ports) != 1 || finding(t, r, "presentation_adapter", 0).Status != Failed || finding(t, r, "discovery_port", 0).Status != Skipped {
		t.Fatal("missing presentation address was not isolated")
	}
}

func TestKnownMGURoutesAndPermissionAreReadOnly(t *testing.T) {
	b, in := fakeBackend(t), operatorInput()
	calls := 0
	b.routes = func(_ context.Context, targets []netip.Addr) (netip.Prefix, error) {
		calls++
		if len(targets) != 1 || targets[0] != in.Runtime.Direct.Address {
			t.Fatal("wrong target")
		}
		return netip.Prefix{}, tunadapter.ErrTargetConflict
	}
	r := run(t.Context(), in, b)
	if calls != 0 || finding(t, r, "mgu_routes", 0).Code != "mgu_not_discovered" {
		t.Fatal("queried an unknown MGU")
	}
	in.Runtime.Direct.Address = netip.MustParseAddr("169.254.91.3")
	in.Runtime.Direct.Phase = app.DirectReady
	r = run(t.Context(), in, b)
	if calls != 0 || finding(t, r, "mgu_routes", 0).Code != "direct_active" {
		t.Fatal("checked owned routes")
	}
	in.Runtime.Direct.Phase = app.DirectDisabled
	r = run(t.Context(), in, b)
	if calls != 1 || finding(t, r, "mgu_routes", 0).Code != "target_conflict" || finding(t, r, "mgu_permission", 0).Code != "permission_available" {
		t.Fatal("missing read-only result")
	}
	in.Permission.Platform = "unobserved"
	r = run(t.Context(), in, b)
	if finding(t, r, "mgu_permission", 0).Status != Skipped {
		t.Fatal("invented permission state")
	}
}

func TestFailuresAreFixedAndCancelJoins(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{syscall.EADDRINUSE, "port_in_use"}, {syscall.EADDRNOTAVAIL, "address_unavailable"}, {syscall.ECONNREFUSED, "connection_refused"},
		{os.ErrPermission, "permission_denied"}, {&net.DNSError{Err: "SECRET", Name: "secret.invalid"}, "dns_failed"}, {errors.New("SECRET"), "network_failed"},
		{&net.DNSError{Err: "SECRET", IsTimeout: true}, "probe_timeout"},
	} {
		if got := networkCode(tc.err); got != tc.code {
			t.Errorf("got %s want %s", got, tc.code)
		}
	}
	synctest.Test(t, func(t *testing.T) {
		b := fakeBackend(t)
		finished := false
		b.lookup = func(ctx context.Context, _ string) ([]netip.Addr, error) {
			<-ctx.Done()
			finished = true
			return nil, ctx.Err()
		}
		in := Input{Config: settings.Default(), Endpoints: []transport.Endpoint{{Kind: "hub", Hub: identity.Fingerprint{1}, Addr: "secret.invalid:22"}}}
		ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
		defer cancel()
		r := run(ctx, in, b)
		if !finished || !r.TimedOut || r.Complete || finding(t, r, "hub_dns", 1).Code != "probe_timeout" {
			t.Fatalf("unjoined deadline: %+v", r)
		}
		data, err := JSON(r)
		if err != nil || strings.Contains(string(data), "secret") {
			t.Fatalf("secret or invalid report: %s %v", data, err)
		}
		cancelled, stop := context.WithCancel(t.Context())
		stop()
		b.interfaces = func() ([]carside.NIC, error) { t.Fatal("pre-canceled backend invoked"); return nil, nil }
		r = run(cancelled, in, b)
		if !r.Canceled || r.Complete || len(r.Findings) != 0 {
			t.Fatalf("pre-cancel: %+v", r)
		}
	})
}

func TestRealLoopbackTCPDoesNotSendApplicationData(t *testing.T) {
	for _, network := range []string{"tcp4", "tcp6"} {
		t.Run(network, func(t *testing.T) {
			address := "127.0.0.1:0"
			if network == "tcp6" {
				address = "[::1]:0"
			}
			listener, err := net.Listen(network, address)
			if err != nil {
				if network == "tcp6" {
					t.Skip("IPv6 loopback is unavailable")
				}
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
				var p [1]byte
				n, err := conn.Read(p[:])
				if n != 0 || err != io.EOF {
					done <- errors.New("probe sent bytes or left socket open")
					return
				}
				done <- nil
			}()
			b := fakeBackend(t)
			b.dial = systemBackend().dial
			b.lookup = func(context.Context, string) ([]netip.Addr, error) {
				t.Fatal("literal endpoint used DNS")
				return nil, nil
			}
			in := Input{Config: settings.Default(), Endpoints: []transport.Endpoint{{Kind: "hub", Hub: identity.Fingerprint{1}, Addr: listener.Addr().String()}}}
			r := run(t.Context(), in, b)
			if finding(t, r, "hub_tcp", 1).Code != "tcp_reachable" {
				t.Fatalf("TCP: %+v", r)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := probePort(t.Context(), network, listener.Addr().String()); networkCode(err) != "port_in_use" {
				t.Fatalf("TCP collision: %v", err)
			}
			udp, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer udp.Close()
			if err := probePort(t.Context(), "udp4", udp.LocalAddr().String()); networkCode(err) != "port_in_use" {
				t.Fatalf("UDP collision: %v", err)
			}
			if err := probePort(t.Context(), "tcp4", "127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
			if err := probePort(t.Context(), "udp4", "127.0.0.1:0"); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEndpointValidationAndReportProjection(t *testing.T) {
	b := fakeBackend(t)
	for _, address := range []string{"[::1]:47000", "127.0.0.1:47000"} {
		status, code := resolve(t.Context(), b, address)
		if status != Pass || code != "literal_address" {
			t.Fatalf("literal %s: %s/%s", address, status, code)
		}
	}
	for _, address := range []string{"", "invalid", "0.0.0.0:22", "[::]:22", "224.0.0.1:22", "x:0", "x:65536"} {
		if status, _ := resolve(t.Context(), b, address); status != Failed {
			t.Fatalf("accepted %s", address)
		}
	}
	now := time.Now()
	r := Report{StartedAt: now, FinishedAt: now, Complete: true, Findings: []Finding{{ID: "SECRET", Code: "SECRET", Status: "SECRET", Index: 999, Duration: time.Millisecond}}}
	data, err := JSON(r)
	if err != nil || strings.Contains(string(data), "SECRET") {
		t.Fatalf("projection: %s %v", data, err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || len(out) != 8 || out["schema"] != "checkup/v1" {
		t.Fatalf("schema: %s %v", data, err)
	}
	if r.Findings[0].ID != "SECRET" {
		t.Fatal("mutated report input")
	}
}

func TestDirectInputCheckIsLocal(t *testing.T) {
	const secret = "00001234"
	b := backend{
		lookup: func(context.Context, string) ([]netip.Addr, error) {
			t.Fatal("direct input triggered DNS")
			return nil, nil
		},
		dial: func(context.Context, string) (io.Closer, error) {
			t.Fatal("direct input triggered TCP")
			return nil, nil
		},
	}
	for _, tc := range []struct {
		code   string
		status Status
	}{
		{DirectInputValid, Pass},
		{DirectInputInvalidHost, Failed},
		{DirectInputInvalidPort, Failed},
		{DirectInputInvalidCode, Failed},
		{DirectInputAddressRequired, Warning},
	} {
		t.Run(tc.code, func(t *testing.T) {
			in := Input{Config: settings.Default(), PeerCode: tc.code}
			// Even stale pinned endpoints must not be probed for direct input.
			in.Endpoints = []transport.Endpoint{{Kind: "hub", Addr: "fixture.invalid:47000", Hub: identity.Fingerprint{1}}}
			r := run(t.Context(), in, b)
			f := finding(t, r, "peer_configuration", 0)
			if !r.Complete || len(r.Findings) != 2 || f.Status != tc.status || f.Code != tc.code {
				t.Fatal("unexpected local finding")
			}
			output, err := JSON(r)
			if err != nil || !strings.Contains(string(output), tc.code) || strings.Contains(string(output), secret) {
				t.Fatal("unexpected exported finding")
			}
		})
	}
}

func TestStandaloneHubProbeSendsNoApplicationData(t *testing.T) {
	local, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range []bool{true, false} {
		ln, err := tls.Listen("tcp4", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{local.Cert}, NextProtos: []string{hub.ALPN}, ClientAuth: tls.RequireAnyClientCert, MinVersion: tls.VersionTLS13})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			conn, err := ln.Accept()
			if err != nil {
				done <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
			var first [1]byte
			n, err := conn.Read(first[:])
			if n != 0 || err == nil || match && err != io.EOF {
				done <- errors.New("unexpected TLS application data or termination")
				return
			}
			done <- nil
		}()
		pin := local.Fingerprint()
		if !match {
			pin[0] ^= 1
		}
		ep := transport.Endpoint{Kind: "hub", Addr: ln.Addr().String(), Hub: pin, Ticket: "full-ticket-12345678"}
		r := run(t.Context(), Input{Config: settings.Default(), PeerID: identity.Fingerprint{2}, Endpoints: []transport.Endpoint{ep}}, fakeBackend(t))
		_ = ln.Close()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		f := finding(t, r, "hub_tls", 1)
		if match && f.Code != "hub_verified" || !match && f.Code != "hub_identity_mismatch" {
			t.Fatalf("wrong pin result: %+v", f)
		}
		data, err := JSON(r)
		if err != nil || strings.Contains(string(data), ep.Ticket) || strings.Contains(string(data), "12345678") {
			t.Fatal("credential in checkup export")
		}
	}
}

func TestOperatorHubModeChecksShareAddress(t *testing.T) {
	// Hub.Addr is a closed loopback port so the hub_tls probe stays local.
	for _, tc := range []struct{ share, want string }{
		{"hub-share.example:47100", "hub-share.example"},
		{"", ""},
	} {
		b, in := fakeBackend(t), operatorInput()
		in.Config.Operator.Mode = "hub"
		in.Config.Operator.Endpoint = "old-direct.example:47000"
		in.Config.Operator.Hub.Addr = "127.0.0.1:9"
		in.Config.Operator.Hub.Share = tc.share
		in.Config.Operator.Hub.FP = identity.Fingerprint{1}.String()
		var hosts []string
		b.lookup = func(_ context.Context, host string) ([]netip.Addr, error) {
			hosts = append(hosts, host)
			return []netip.Addr{netip.MustParseAddr("192.0.2.10")}, nil
		}
		run(t.Context(), in, b)
		if slices.Contains(hosts, "old-direct.example") || tc.want != "" && !slices.Contains(hosts, tc.want) || tc.want == "" && len(hosts) != 0 {
			t.Fatalf("share %q resolved %v", tc.share, hosts)
		}
	}
}

func TestCenterChecks(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		in := operatorInput()
		in.Config.Role = role
		in.Config.Operator.Mode = "center"
		in.Config.Center = settings.Center{ID: "test-center", API: []string{"https://localhost"}}
		in.PeerCode = CenterInput
		in.AppVersion = "1.0.0"
		b := fakeBackend(t)
		metaCalls, accountCalls := 0, 0
		b.meta = func(_ context.Context, cfg settings.Center, r, v string) []center.EntryResult {
			metaCalls++
			if cfg.ID != in.Config.Center.ID || r != role || v != in.AppVersion {
				t.Fatal("wrong meta request")
			}
			return []center.EntryResult{{Status: center.Verified, Entry: "secret-entry"}}
		}
		in.AccountStatus = func(context.Context) error { accountCalls++; return centerflow.Unauthenticated }
		report := run(t.Context(), in, b)
		if metaCalls != 1 || finding(t, report, "center_meta", 0).Status != Pass {
			t.Fatal("missing metadata check")
		}
		if role == settings.RoleClient && (accountCalls != 0 || len(report.Findings) != 2) {
			t.Fatal("bare code triggered additional probes")
		}
		if role == settings.RoleOperator && (accountCalls != 1 || finding(t, report, "center_login", 0).Code != "unauthenticated") {
			t.Fatal("operator login not checked")
		}
		data, err := JSON(report)
		if err != nil || strings.Contains(string(data), "secret-entry") {
			t.Fatal("unsafe checkup output")
		}
	}
}
