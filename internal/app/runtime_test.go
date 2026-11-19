package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"remote-enet/internal/carside"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/invite"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/operator"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/fakezgw"
)

func freeTCPAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func localPorts(t *testing.T) operator.Ports {
	t.Helper()
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
	return operator.Ports{Diagnostic: diag.Addr().(*net.TCPAddr).Port, Control: ctrl.Addr().(*net.TCPAddr).Port, Discovery: disc.LocalAddr().(*net.UDPAddr).Port}
}

func sendDiagnostic(t *testing.T, addr netip.AddrPort) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp4", addr.String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	req := hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}
	if err := hsfz.WriteFrame(c, req); err != nil {
		t.Fatal(err)
	}
	ack, err := hsfz.ReadFrame(c, 1024)
	if err != nil || ack.Control != hsfz.ControlAck {
		t.Fatalf("missing transparent ACK: %+v %v", ack, err)
	}
	return c
}

func portsReleased(t *testing.T, s Snapshot) {
	t.Helper()
	for _, addr := range []netip.AddrPort{s.Diagnostic, s.Control} {
		ln, err := net.Listen("tcp4", addr.String())
		if err != nil {
			t.Fatalf("old presenter port is occupied at %s: %v", addr, err)
		}
		ln.Close()
	}
	disc, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(s.Discovery))
	if err != nil {
		t.Fatalf("old discovery port is occupied: %v", err)
	}
	disc.Close()
}

func TestPublicRuntimesReconnectAndReleasePresenter(t *testing.T) {
	recorder := hsfzcapture.New(hsfzcapture.Options{})
	capturePath := filepath.Join(t.TempDir(), "runtime.jsonl")
	if err := recorder.Start(t.Context(), capturePath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Stop(context.Background()) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	nic, gateway := gatewayState(t)
	zgw, err := fakezgw.Start(ctx, gateway.Address, fakezgw.Profile{Ident: gateway.Identification}, fakezgw.WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer zgw.Close()
	remoteDiag, remoteCtrl, _ := zgw.Addr()
	cfg := OperatorConfig{Capture: recorder, StateDir: t.TempDir(), Listen: freeTCPAddress(t), Bind: nic.Address, Ports: localPorts(t), RemoteDiagnosticPort: uint16(remoteDiag.Port), RemoteControlPort: uint16(remoteCtrl.Port), Logger: quietLogger(), AllowLinkTest: true, Exclusive: true, Resolver: ownerResolver{owner: exclusive.Identity{PID: 123, Created: 456}}}
	op := New()
	opCtx, stopOp := context.WithCancel(ctx)
	defer stopOp()
	opStates := op.Subscribe(ctx)
	opDone := make(chan error, 1)
	go func() { opDone <- op.RunOperator(opCtx, cfg) }()
	t.Cleanup(func() {
		stopOp()
		select {
		case err := <-opDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("operator stop: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("operator did not join")
		}
	})
	snapshotWhere(t, opStates, func(s Snapshot) bool { return s.Phase == PhaseListening })
	code, err := IssueInvite(ctx, InviteConfig{StateDir: cfg.StateDir, Endpoint: cfg.Listen, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	invitation, err := invite.Decode(code)
	if err != nil {
		t.Fatal(err)
	}
	n := networkForTest(nic, gateway)
	clientCfg := ClientConfig{StateDir: t.TempDir(), Invite: code, AllowLinkTest: true, NICName: nic.Name, NICCheckInterval: time.Hour, Logger: quietLogger(), Hooks: ClientHooks{Interfaces: n.interfaces, Discover: n.discover}}
	clientCfg.Hooks.Wait = n.manualNICWait(clientCfg.NICCheckInterval)
	client := New()
	clientCtx, stopClient := context.WithCancel(ctx)
	defer stopClient()
	clientStates := client.Subscribe(ctx)
	clientDone := make(chan error, 1)
	go func() { clientDone <- client.RunClient(clientCtx, clientCfg) }()
	t.Cleanup(func() {
		stopClient()
		select {
		case err := <-clientDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("client stop: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("client did not join")
		}
	})
	firstClient := connectedGeneration(t, clientStates, 1)
	firstOp := connectedGeneration(t, opStates, 1)
	if firstClient.Peer != invitation.Peer.ID || firstOp.Peer != firstClient.Device || firstClient.Path.Kind != "direct" {
		t.Fatalf("wrong paired path: client=%+v operator=%+v", firstClient, firstOp)
	}

	options := linktest.Options{Samples: 3, PayloadBytes: 4096, FrameBytes: 512, Timeout: time.Second}
	measured, err := op.TestLink(ctx, options)
	if err != nil || measured.Samples != 3 || measured.BytesSent != 4096 || measured.BytesReceived != 4096 {
		t.Fatalf("app connection test: %+v %v", measured, err)
	}
	if stats := zgw.Stats(); stats.Connections != 0 || stats.DiagReceived != 0 {
		t.Fatalf("link test reached vehicle: %+v", stats)
	}
	if metrics := op.Snapshot().Metrics; metrics.ActiveConnections != 0 || metrics.Totals != (observe.Bytes{}) {
		t.Fatalf("link-test bytes counted as vehicle payload: %+v", metrics)
	}
	if recorder.Snapshot().ObservedBytes != 0 {
		t.Fatal("peer-only test or session handshake entered capture")
	}
	a, b := sendDiagnostic(t, firstOp.Diagnostic), sendDiagnostic(t, firstOp.Diagnostic)
	if zgw.Stats().DiagReceived != 2 {
		t.Fatal("concurrent diagnostic streams not forwarded")
	}

	firstMetrics := awaitMetrics(t, op, func(s Snapshot) bool {
		return s.Metrics.Session.Bytes == (observe.Bytes{In: 22, Out: 22}) && s.Metrics.ActiveConnections == 2 && s.Exclusive.Active == 2
	})
	if firstMetrics.Exclusive.Owner != (exclusive.Identity{PID: 123, Created: 456}) || firstMetrics.Metrics.Rate.InBytesPerSecond <= 0 || firstMetrics.Metrics.Rate.OutBytesPerSecond <= 0 {
		t.Fatalf("missing live metrics/owner: %+v", firstMetrics)
	}
	awaitMetrics(t, client, func(s Snapshot) bool {
		return s.Metrics.Session.Bytes == (observe.Bytes{In: 22, Out: 22}) && s.Metrics.ActiveConnections == 2
	})
	partial, err := op.TestLink(ctx, linktest.Options{Samples: 1024, PayloadBytes: linktest.MaxPayloadBytes, Timeout: time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) || !partial.TimedOut {
		t.Fatalf("test timeout reported as success: %+v %v", partial, err)
	}
	if stats := zgw.Stats(); stats.Connections != 2 || stats.DiagReceived != 2 {
		t.Fatalf("failed connection test reached vehicle: %+v", stats)
	}
	// The canceled peer-only test must leave both ordinary streams usable.
	for _, c := range []net.Conn{a, b} {
		if err := hsfz.WriteFrame(c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}); err != nil {
			t.Fatal(err)
		}
		if frame, err := hsfz.ReadFrame(c, 1024); err != nil || frame.Control != hsfz.ControlAck {
			t.Fatalf("ordinary tunnel damaged by test cancellation: %+v %v", frame, err)
		}
	}
	// A source-prefix change must release fixed presenter ports and reconnect
	// automatically, using the identity trusted by the consumed invitation.
	monitor := n.nextWait(t, clientCfg.NICCheckInterval)
	nic.Prefix = netip.MustParsePrefix("127.0.0.0/16")
	n.setNICs(nic)
	close(monitor.release)
	secondClient := connectedGeneration(t, clientStates, 2)
	secondOp := connectedGeneration(t, opStates, 2)
	if secondClient.NIC != nic || firstOp.Diagnostic != secondOp.Diagnostic || firstOp.Control != secondOp.Control || firstOp.Discovery != secondOp.Discovery {
		t.Fatalf("presenter failed to rebind cleanly: first=%+v next=%+v", firstOp, secondOp)
	}
	for _, c := range []net.Conn{a, b} {
		c.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("old diagnostic TCP survived reconnect")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("old diagnostic TCP was left blocked")
		}
	}
	if zgw.Stats().DiagReceived != 4 {
		t.Fatal("old diagnostic requests replayed")
	}

	if metrics := op.Snapshot().Metrics; metrics.Session.Bytes != (observe.Bytes{}) || metrics.Session.Generation != 2 || metrics.ActiveConnections != 0 || len(metrics.History) != 2 {
		t.Fatalf("old measurements survived generation: %+v", metrics)
	}
	if secondOp.Exclusive.Active != 0 || secondOp.Exclusive.Owner != (exclusive.Identity{}) {
		t.Fatalf("old exclusive owner survived generation: %+v", secondOp.Exclusive)
	}
	if measured, err := op.TestLinkForGeneration(ctx, 1, options); !errors.Is(err, ErrNotConnected) || measured.BytesSent != 0 {
		t.Fatalf("stale test ran on replacement session: %+v %v", measured, err)
	}
	if measured, err := op.TestLink(ctx, options); err != nil || measured.Samples != 3 {
		t.Fatalf("second-generation link test: %+v %v", measured, err)
	}
	_ = sendDiagnostic(t, secondOp.Diagnostic)
	rtt := snapshotWhere(t, clientStates, func(s Snapshot) bool { return s.Phase == PhaseConnected && s.Generation == 2 && s.RTTValid })
	if !rtt.RTTValid || rtt.Metrics.Session.RTT.TotalSamples == 0 || rtt.RTT < 0 {
		t.Fatalf("invalid measured RTT: generation=%d valid=%t rtt=%s samples=%+v", rtt.Generation, rtt.RTTValid, rtt.RTT, rtt.Metrics.Session.RTT)
	}
	stopClient()
	ended := snapshotWhere(t, opStates, func(s Snapshot) bool { return s.Phase == PhaseListening && s.Generation == 2 })
	if !ended.Peer.IsZero() || ended.NIC != (carside.NIC{}) || ended.Gateway.IsValid() || ended.VIN != "" {
		t.Errorf("operator retained client or vehicle after disconnect: %+v", ended)
	}
	if ended.LastError != "Client disconnected." {
		t.Errorf("disconnect message: %q", ended.LastError)
	}
	portsReleased(t, secondOp)
	if s := op.Snapshot(); s.Diagnostic.IsValid() || s.Control.IsValid() || s.Discovery.IsValid() {
		t.Fatalf("released endpoints retained in snapshot: %+v", s)
	}
	if err := recorder.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	metadata := map[uint64]hsfzcapture.Metadata{}
	captured := map[uint64]map[hsfzcapture.Direction][]byte{}
	generations := map[uint64]int{}
	var frames int
	for _, e := range queryCaptureEvents(t, capturePath) {
		if e.Type == "connection_start" {
			metadata[e.Connection] = *e.Metadata
			generations[e.Metadata.Generation]++
			captured[e.Connection] = map[hsfzcapture.Direction][]byte{}
			if e.Metadata.Channel != "diagnostic" || e.Metadata.Target != remoteDiag.String() {
				t.Fatalf("runtime capture metadata: %+v", e.Metadata)
			}
		}
		if e.Type == "data" {
			b, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			captured[e.Connection][e.Direction] = append(captured[e.Connection][e.Direction], b...)
		}
		if e.Type == "frame" {
			frames++
			if e.Frame.Generation != metadata[e.Connection].Generation {
				t.Fatal("frame crossed generation")
			}
		}
	}
	if generations[1] != 2 || generations[2] != 1 || len(metadata) != 3 || frames != 10 {
		t.Fatalf("runtime capture generations=%v connections=%d frames=%d", generations, len(metadata), frames)
	}
	request, _ := (hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}).MarshalBinary()
	ack, _ := (hsfz.Frame{Control: hsfz.ControlAck, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}).MarshalBinary()
	for id, m := range metadata {
		repetitions := 1
		if m.Generation == 1 {
			repetitions = 2
		}
		if !bytes.Equal(captured[id][hsfzcapture.ToVehicle], bytes.Repeat(request, repetitions)) || !bytes.Equal(captured[id][hsfzcapture.FromVehicle], bytes.Repeat(ack, repetitions)) {
			t.Fatalf("runtime capture bytes for %d: %x", id, captured[id])
		}
	}
	// Closed client sockets must be readable as EOF/errors rather than waiting
	// for a later session to supply a response.
	for _, c := range []net.Conn{a, b} {
		_, _ = io.Copy(io.Discard, c)
	}
}

func TestSessionEndMessage(t *testing.T) {
	for _, test := range []struct {
		err      error
		operator string
		client   string
	}{
		{io.EOF, "Client disconnected.", "Operator disconnected."},
		{yamux.ErrSessionShutdown, "Client disconnected.", "Operator disconnected."},
		{syscall.ECONNRESET, "Client disconnected.", "Operator disconnected."},
		{net.ErrClosed, "Client disconnected.", "Operator disconnected."},
		{context.Canceled, "Client disconnected.", "Operator disconnected."},
		{session.ErrPairing, "Pairing rejected.", "Pairing rejected."},
		{session.ErrVersion, "Incompatible protocol version.", "Incompatible protocol version."},
		{errors.New("unexpected transport detail"), "Client session ended with an error.", "Operator session ended with an error."},
	} {
		for role, want := range map[session.Role]string{session.RoleOperator: test.operator, session.RoleClient: test.client} {
			if got := sessionEndMessage(role, test.err); got != want {
				t.Errorf("%s error %v: got %q, want %q", role, test.err, got, want)
			}
		}
	}
}

func awaitMetrics(t *testing.T, r *Runtime, match func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s := r.Snapshot()
		if match(s) {
			return s
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatalf("expected metrics not received: %+v", s)
			return Snapshot{}
		}
	}
}
