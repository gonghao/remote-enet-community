package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/testutil/fakezgw"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestHelpIsSuccessful(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"help"}, {"invite", "--help"}, {"operator", "--help"}, {"client", "--help"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, &stdout, &stderr); err != nil {
				t.Fatal(err)
			}
			if stdout.Len()+stderr.Len() == 0 {
				t.Fatal("help was empty")
			}
		})
	}
}

func TestInviteWritesOnlyCodeAndPersistsIdentity(t *testing.T) {
	dir := t.TempDir()
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"invite", "--state-dir", dir, "--endpoint", "localhost:47000"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	code := strings.TrimSpace(stdout.String())
	i, err := invite.Decode(code)
	if err != nil || len(strings.Fields(code)) != 1 {
		t.Fatalf("stdout is not one invitation: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("unexpected invite log: %s", stderr.String())
	}
	store, err := trust.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	id, err := store.LoadIdentity(t.Context())
	if err != nil || id.Fingerprint() != i.Peer.ID || i.Peer.Endpoints[0].Hub != i.Peer.ID {
		t.Fatal("invitation did not pin the persisted identity")
	}
	if ok, err := store.Consume(t.Context(), identity.Fingerprint{1}, i.Token); !ok || err != nil {
		t.Fatalf("invitation not usable: %t %v", ok, err)
	}
	if ok, err := store.Consume(t.Context(), identity.Fingerprint{2}, i.Token); ok || err != nil {
		t.Fatalf("invitation reusable: %t %v", ok, err)
	}
}

func TestInvalidArguments(t *testing.T) {
	for _, args := range [][]string{
		{}, {"unknown"}, {"invite"}, {"invite", "--endpoint", "localhost:0"}, {"invite", "--endpoint", "0.0.0.0:1"}, {"invite", "--endpoint", "localhost:1", "--ttl", "1ms"},
		{"client"}, {"client", "--invite", "secret", "--peer", "other"}, {"client", "--invite", "secret", "--nic-addr", "::1"}, {"client", "--invite", "secret", "--discovery-target", "localhost:6811"}, {"client", "--invite", "secret", "--discovery-timeout", "0"},
		{"client", "--invite", "secret", "--discovery-retry", "0"}, {"client", "--invite", "secret", "--nic-check", "-1s"},
		{"client", "--invite", "secret", "--status-interval", "-1s"}, {"client", "--invite", "secret", "--connection-test", "--allow-link-test=false"}, {"client", "--invite", "secret", "--connection-test-timeout", "16s"},
		{"operator"}, {"operator", "--bind", "0.0.0.0"}, {"operator", "--bind", "127.0.0.1", "--listen", "localhost:0"}, {"operator", "--bind", "127.0.0.1", "--diag-port", "-1"}, {"operator", "--bind", "127.0.0.1", "--remote-ctrl-port", "65536"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			if len(args) > 1 {
				args = append(append([]string(nil), args...), "--state-dir", t.TempDir())
			}
			var stdout, stderr bytes.Buffer
			if err := run(t.Context(), args, &stdout, &stderr); err == nil {
				t.Fatal("invalid arguments accepted")
			}
			if strings.Contains(stderr.String(), "secret") {
				t.Fatal("invitation leaked to diagnostic output")
			}
		})
	}
}

func gatewayState(t *testing.T) (carside.NIC, carside.Gateway) {
	t.Helper()
	id := hsfz.Identification{DiagAddr: 0x10, VIN: "WBAXXXXXXX0000001", MAC: [6]byte{1, 2, 3, 4, 5, 6}}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: id.MarshalBody()}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return carside.NIC{Name: "test", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}, carside.Gateway{Address: netip.MustParseAddr("127.0.0.2"), Identification: id, IdentRaw: raw}
}

type retryLog struct {
	mu       sync.Mutex
	buf      bytes.Buffer
	once     sync.Once
	retrying chan struct{}
}

func (w *retryLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.buf.Write(p)
	if bytes.Contains(p, []byte("retrying")) {
		w.once.Do(func() { close(w.retrying) })
	}
	return n, err
}

func TestNetworkRetryIsCancelable(t *testing.T) {
	_, gateway := gatewayState(t)
	zgw, err := fakezgw.Start(t.Context(), gateway.Address, fakezgw.Profile{Ident: gateway.Identification}, fakezgw.WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer zgw.Close()
	_, _, discovery := zgw.Addr()
	closed, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := closed.Addr().String()
	closed.Close()
	peer := identity.Fingerprint{2}
	code, err := (invite.Invite{Version: 1, Peer: transport.PeerInfo{ID: peer, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: endpoint, Hub: peer}}}, Token: [16]byte{3}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	logs := &retryLog{retrying: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{"client", "--state-dir", t.TempDir(), "--invite", code, "--nic-addr", "127.0.0.1", "--discovery-target", discovery.String(), "--discovery-timeout", "10ms"}, io.Discard, logs)
	}()
	select {
	case <-logs.retrying:
	case err := <-done:
		t.Fatalf("client returned before retry: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("network retry not observed")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("retry ignored cancellation")
	}
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if bytes.Contains(logs.buf.Bytes(), []byte(code)) {
		t.Fatal("logs contain invitation")
	}
}

func TestPairFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	direct, err := readPairFile(write("ok", "192.0.2.107:47000 0482 7193\n"))
	if err != nil || direct.Address != "192.0.2.107:47000" || direct.Code != "04827193" {
		t.Fatalf("direct pair = %+v, %v", direct, err)
	}
	if _, err := readPairFile(write("oversized", "192.0.2.107:47000 04827193"+strings.Repeat(" ", 4096))); err == nil || err.Error() != "--pair-file exceeds 4 KiB" {
		t.Fatalf("oversized pair file: %v", err)
	}
	for name, content := range map[string]string{"code-only": "0482 7193", "bad-code": "192.0.2.107:47000 0482719", "bad-host": "bad_host:47000 04827193"} {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(t.Context(), []string{"client", "--state-dir", t.TempDir(), "--pair-file", write(name, content)}, &stdout, &stderr)
			if err == nil {
				t.Fatal("invalid pair file accepted")
			}
			if strings.Contains(err.Error()+stderr.String(), "0482") {
				t.Fatal("numeric code leaked")
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"client", "--state-dir", t.TempDir(), "--pair-file", write("ok2", "192.0.2.107:47000 04827193"), "--peer", "x"}, &stdout, &stderr); err == nil {
		t.Fatal("multiple pairing sources accepted")
	}
}

func TestPairFileReadFailureDoesNotEchoPath(t *testing.T) {
	for _, args := range [][]string{{"0482 7193"}, {"0482", "7193"}, {filepath.Join(t.TempDir(), "04827193")}} {
		var stdout, stderr bytes.Buffer
		flags := append([]string{"client", "--state-dir", t.TempDir(), "--pair-file"}, args...)
		err := run(t.Context(), flags, &stdout, &stderr)
		if err == nil {
			t.Fatal("invalid --pair-file path accepted")
		}
		output := err.Error() + stdout.String() + stderr.String()
		if strings.Contains(output, "0482") || strings.Contains(output, "7193") {
			t.Fatal("numeric code leaked from --pair-file path")
		}
	}
}
