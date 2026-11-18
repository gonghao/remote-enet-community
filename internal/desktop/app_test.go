package desktop

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/invite"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/settings"
	"remote-enet/internal/testutil/fakezgw"
)

// Only endpoint selection is injected. Both Run methods, subscriptions,
// pairing, TLS, link tests, discovery and transparent forwarding are real.
type loopbackRuntime struct {
	*app.Runtime
	nic                 carside.NIC
	discovery           netip.AddrPort
	diagnostic, control uint16
}

func (r *loopbackRuntime) RunClient(ctx context.Context, cfg app.ClientConfig) error {
	cfg.Hooks.Interfaces = func(ctx context.Context) ([]carside.NIC, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return []carside.NIC{r.nic}, nil
	}
	cfg.DiscoveryDestinations = []netip.AddrPort{r.discovery}
	cfg.DiscoveryTimeout = 50 * time.Millisecond
	return r.Runtime.RunClient(ctx, cfg)
}

func (r *loopbackRuntime) RunOperator(ctx context.Context, cfg app.OperatorConfig) error {
	cfg.RemoteDiagnosticPort, cfg.RemoteControlPort = r.diagnostic, r.control
	return r.Runtime.RunOperator(ctx, cfg)
}

func tcpAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}

func loopbackOperatorSettings(t *testing.T) settings.Config {
	t.Helper()
	cfg := operatorSettings()
	cfg.Operator.Listen = tcpAddress(t)
	cfg.Operator.Endpoint = cfg.Operator.Listen
	// Hold both TCP ports until they have been selected so they cannot alias.
	diag, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer diag.Close()
	ctrl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ctrl.Close()
	disc, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer disc.Close()
	cfg.Operator.DiagnosticPort = diag.Addr().(*net.TCPAddr).Port
	cfg.Operator.ControlPort = ctrl.Addr().(*net.TCPAddr).Port
	cfg.Operator.DiscoveryPort = disc.LocalAddr().(*net.UDPAddr).Port
	return cfg
}

func realController(t *testing.T, cfg settings.Config, factory func() appRuntime) *Controller {
	t.Helper()
	dir := t.TempDir()
	opts := Options{ConfigPath: filepath.Join(dir, "settings.json"), StateDir: filepath.Join(dir, "state")}
	if err := settings.Save(opts.ConfigPath, cfg); err != nil {
		t.Fatal(err)
	}
	var c *Controller
	var err error
	if factory == nil {
		c, err = New(t.Context(), opts)
	} else {
		deps := production()
		deps.newRuntime = factory
		c, err = newController(t.Context(), opts, deps)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("close real controller: %v", err)
		}
	})
	return c
}

func assertTCPReleased(t *testing.T, addr string) {
	t.Helper()
	ln, err := net.Listen("tcp4", addr)
	if err != nil {
		t.Fatalf("port not released at %s: %v", addr, err)
	}
	ln.Close()
}

func assertPresenterReleased(t *testing.T, s app.Snapshot) {
	t.Helper()
	assertTCPReleased(t, s.Diagnostic.String())
	assertTCPReleased(t, s.Control.String())
	conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(s.Discovery))
	if err != nil {
		t.Fatalf("discovery port not released: %v", err)
	}
	conn.Close()
}

func TestProductionOperatorCanIssueWhileListeningAndRestart(t *testing.T) {
	cfg := loopbackOperatorSettings(t)
	c := realController(t, cfg, nil)
	request, cancel := context.WithCancel(t.Context())
	if err := c.StartOperator(request); err != nil {
		t.Fatal(err)
	}
	cancel()
	first := stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseListening })
	code, err := c.IssueInvite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i, err := invite.Decode(code)
	if err != nil || i.Peer.ID != first.Runtime.Device || i.Peer.Endpoints[0].Addr != cfg.Operator.Endpoint || time.Until(i.ExpiresAt) < 14*time.Minute || time.Until(i.ExpiresAt) > 15*time.Minute {
		t.Fatalf("invitation did not use active operator identity, endpoint and TTL: %v", err)
	}
	if err := c.Save(t.Context(), cfg); !errors.Is(err, ErrBusy) {
		t.Fatal("configuration changed while operator was listening")
	}
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertTCPReleased(t, cfg.Operator.Listen)
	if c.Snapshot().Invitation != "" {
		t.Fatal("stop retained the invitation")
	}
	if err := c.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	second := stateWhere(t, c, func(s State) bool { return s.Runtime.Phase == app.PhaseListening })
	if first.Runtime.Device != second.Runtime.Device || first.Runtime.Device.IsZero() {
		t.Fatal("operator restart minted a different identity")
	}
	if err := c.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertTCPReleased(t, cfg.Operator.Listen)
	assertNoSecretOnDisk(t, filepath.Dir(c.opts.ConfigPath), code)
}

const loopbackGatewayAddress = "127.0.0.2"

// The gateway must bind an unassigned address: configuring an alias would make
// carside reject it as a local target. Only this exact bind failure may skip.
func loopbackFixtureUnavailable(err error) bool {
	var op *net.OpError
	var call *os.SyscallError
	if !errors.As(err, &op) || op.Op != "listen" || !errors.As(op.Err, &call) || call.Syscall != "bind" || !errors.Is(call.Err, syscall.EADDRNOTAVAIL) {
		return false
	}
	ip := net.ParseIP(loopbackGatewayAddress)
	switch addr := op.Addr.(type) {
	case *net.TCPAddr:
		return addr != nil && (op.Net == "tcp" || op.Net == "tcp4") && addr.IP.Equal(ip) && addr.Port == 0 && addr.Zone == ""
	case *net.UDPAddr:
		return addr != nil && (op.Net == "udp" || op.Net == "udp4") && addr.IP.Equal(ip) && addr.Port == 0 && addr.Zone == ""
	default:
		return false
	}
}

func TestLoopbackFixtureUnavailable(t *testing.T) {
	tcp := &net.TCPAddr{IP: net.ParseIP(loopbackGatewayAddress)}
	udp := &net.UDPAddr{IP: net.ParseIP(loopbackGatewayAddress)}
	listenError := func(network string, addr net.Addr, err error) error {
		return fmt.Errorf("listen fake gateway: %w", &net.OpError{Op: "listen", Net: network, Addr: addr, Err: err})
	}
	bindError := func(err error) error { return &os.SyscallError{Syscall: "bind", Err: err} }
	unavailable := bindError(syscall.EADDRNOTAVAIL)
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"tcp-unassigned", listenError("tcp", tcp, unavailable), true},
		{"tcp4-unassigned", listenError("tcp4", tcp, unavailable), true},
		{"udp-unassigned", listenError("udp", udp, unavailable), true},
		{"udp4-unassigned", listenError("udp4", udp, unavailable), true},
		{"success", nil, false},
		{"permission", listenError("tcp", tcp, bindError(syscall.EACCES)), false},
		{"operation-not-permitted", listenError("tcp", tcp, bindError(syscall.EPERM)), false},
		{"address-in-use", listenError("tcp", tcp, bindError(syscall.EADDRINUSE)), false},
		{"file-limit", listenError("tcp", tcp, bindError(syscall.EMFILE)), false},
		{"buffer-limit", listenError("udp", udp, bindError(syscall.ENOBUFS)), false},
		{"address-family", listenError("tcp", tcp, bindError(syscall.EAFNOSUPPORT)), false},
		{"canceled", listenError("tcp", tcp, context.Canceled), false},
		{"deadline", listenError("tcp", tcp, context.DeadlineExceeded), false},
		{"text-only", errors.New("listen diagnostic: listen tcp 127.0.0.2:0: bind: can't assign requested address"), false},
		{"local-target", errors.New("carside: target 127.0.0.2:6801 is a local address"), false},
		{"bare-errno", syscall.EADDRNOTAVAIL, false},
		{"bare-bind", unavailable, false},
		{"missing-bind", listenError("tcp", tcp, syscall.EADDRNOTAVAIL), false},
		{"socket-failure", listenError("tcp", tcp, &os.SyscallError{Syscall: "socket", Err: syscall.EADDRNOTAVAIL}), false},
		{"dial-failure", &net.OpError{Op: "dial", Net: "tcp", Addr: tcp, Err: unavailable}, false},
		{"assigned-loopback", listenError("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1")}, unavailable), false},
		{"other-loopback", listenError("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.3")}, unavailable), false},
		{"wildcard", listenError("tcp", &net.TCPAddr{IP: net.IPv4zero}, unavailable), false},
		{"ipv6", listenError("tcp6", &net.TCPAddr{IP: net.IPv6loopback}, unavailable), false},
		{"fixed-port", listenError("tcp", &net.TCPAddr{IP: tcp.IP, Port: 6801}, unavailable), false},
		{"zone", listenError("tcp", &net.TCPAddr{IP: tcp.IP, Zone: "lo0"}, unavailable), false},
		{"missing-address", listenError("tcp", nil, unavailable), false},
		{"nil-tcp-address", listenError("tcp", (*net.TCPAddr)(nil), unavailable), false},
		{"nil-udp-address", listenError("udp", (*net.UDPAddr)(nil), unavailable), false},
		{"wrong-network", listenError("tcp6", tcp, unavailable), false},
		{"wrong-address-type", listenError("tcp", udp, unavailable), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := loopbackFixtureUnavailable(tc.err); got != tc.want {
				t.Fatalf("fixture unavailable = %v, want %v: %v", got, tc.want, tc.err)
			}
		})
	}
}

func TestRealAppsPairRememberReconnectTestAndRelease(t *testing.T) {
	ident := hsfz.Identification{VIN: "WBAXXXXXXX0000001", DiagAddr: 0x10, MAC: [6]byte{1, 2, 3, 4, 5, 6}}
	zgw, err := fakezgw.Start(t.Context(), netip.MustParseAddr(loopbackGatewayAddress), fakezgw.Profile{Ident: ident}, fakezgw.WithPorts(0, 0, 0), fakezgw.WithLogger(quietLogger()))
	if err != nil {
		if loopbackFixtureUnavailable(err) {
			t.Skipf("fixture requires unassigned %s TCP/UDP listeners; host cannot bind it without an address alias, which carside would reject as a local target: %v", loopbackGatewayAddress, err)
		}
		t.Fatal(err)
	}
	defer zgw.Close()
	diag, ctrl, discovery := zgw.Addr()
	op := realController(t, loopbackOperatorSettings(t), func() appRuntime {
		return &loopbackRuntime{Runtime: app.New(), diagnostic: uint16(diag.Port), control: uint16(ctrl.Port)}
	})
	nic := carside.NIC{Name: "loopback-test", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}
	clientCfg := settings.Default()
	clientCfg.Client.NICName, clientCfg.Client.NICAddr = nic.Name, nic.Address.String()
	client := realController(t, clientCfg, func() appRuntime {
		return &loopbackRuntime{Runtime: app.New(), nic: nic, discovery: discovery.AddrPort()}
	})
	if err := op.StartOperator(t.Context()); err != nil {
		t.Fatal(err)
	}
	stateWhere(t, op, func(s State) bool { return s.Runtime.Phase == app.PhaseListening })
	code, err := op.IssueInvite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	i, err := invite.Decode(code)
	if err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(t.Context())
	if err := client.StartClient(request, code); err != nil {
		t.Fatal(err)
	}
	cancel()
	firstClient := stateWhere(t, client, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected && s.Config.Client.Peer != "" })
	firstOp := stateWhere(t, op, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected })
	if firstClient.Config.Client.Peer != i.Peer.ID.String() || firstClient.Runtime.VIN != ident.VIN || firstOp.Runtime.Peer != firstClient.Runtime.Device {
		t.Fatal("real pairing and saved peer did not agree")
	}
	saved, err := settings.Load(client.opts.ConfigPath)
	if err != nil || saved.Client.Peer != firstClient.Config.Client.Peer {
		t.Fatalf("saved GUI peer missing: %v", err)
	}
	for _, c := range []*Controller{client, op} {
		if err := c.TestLink(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := c.Snapshot(); s.TestResult.Samples == 0 || s.TestResult.BytesReceived == 0 || s.Runtime.Metrics.Totals != (observe.Bytes{}) {
			t.Fatal("real peer test lost measurements or counted them as vehicle data")
		}
	}
	if stats := zgw.Stats(); stats.Connections != 0 || stats.DiagReceived != 0 {
		t.Fatal("peer connection tests reached the fake vehicle")
	}
	tool, err := net.DialTimeout("tcp4", firstOp.Runtime.Diagnostic.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()
	if err := tool.SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	frame := hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}
	if err := hsfz.WriteFrame(tool, frame); err != nil {
		t.Fatal(err)
	}
	ack, err := hsfz.ReadFrame(tool, 1024)
	if err != nil || ack.Control != hsfz.ControlAck || !bytes.Equal(ack.Body, frame.Body) {
		t.Fatalf("opaque diagnostic forwarding failed: %v", err)
	}
	if err := client.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Read(make([]byte, 1)); err == nil {
		t.Fatal("old diagnostic connection survived stop")
	} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
		t.Fatal("old diagnostic connection remained blocked")
	}
	stateWhere(t, op, func(s State) bool { return s.Runtime.Phase == app.PhaseListening })
	assertPresenterReleased(t, firstOp.Runtime)
	if err := client.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	secondClient := stateWhere(t, client, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected })
	secondOp := stateWhere(t, op, func(s State) bool {
		return s.Runtime.Phase == app.PhaseConnected && s.Runtime.Generation > firstOp.Runtime.Generation
	})
	if secondClient.Runtime.Device != firstClient.Runtime.Device || secondClient.Runtime.Generation != 1 || secondClient.TestResult != (linktest.Result{}) || secondOp.Runtime.Diagnostic != firstOp.Runtime.Diagnostic {
		t.Fatal("new runtime failed to reuse pairing and fixed presenter ports independently")
	}
	if err := op.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := client.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	assertTCPReleased(t, op.Snapshot().Config.Operator.Listen)
	assertPresenterReleased(t, secondOp.Runtime)
	for _, c := range []*Controller{client, op} {
		assertNoSecretOnDisk(t, filepath.Dir(c.opts.ConfigPath), code)
	}
	trustData, err := os.ReadFile(filepath.Join(client.opts.StateDir, "client", "trust.json"))
	if err != nil || bytes.Contains(trustData, []byte("nic_name")) || bytes.Contains(trustData, []byte("allow_link_test")) {
		t.Fatalf("GUI settings contaminated trust state: %v", err)
	}
}

func assertNoSecretOnDisk(t *testing.T, root, code string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err == nil && (bytes.Contains(raw, []byte(code)) || strings.Contains(strings.ToLower(string(raw)), "renet1:")) {
			t.Errorf("invitation persisted in %s", path)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsCorruptSettingsWithoutStartingRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(dir, "state")
	if _, err := New(t.Context(), Options{ConfigPath: path, StateDir: stateDir}); err == nil {
		t.Fatal("corrupt configuration fell back to defaults")
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed load created runtime state")
	}
}
