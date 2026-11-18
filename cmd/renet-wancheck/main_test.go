package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

func TestDefaultHelpAndVersionDoNotReadInputOrCreateState(t *testing.T) {
	for _, args := range [][]string{nil, {"--help"}, {"--version"}} {
		dir := filepath.Join(t.TempDir(), "missing")
		var output bytes.Buffer
		if code := run(t.Context(), append(args, "--state-dir", dir), nil, &output, io.Discard); code != 0 {
			t.Fatalf("code=%d", code)
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("non-run created state")
		}
	}
}

func TestBadInputDoesNotCreateStateOrLeakSecrets(t *testing.T) {
	for _, code := range []string{"renet1:SECRET", "SECRET"} {
		path := filepath.Join(t.TempDir(), "invite")
		if err := os.WriteFile(path, []byte(code), 0o600); err != nil {
			t.Fatal(err)
		}
		input, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(t.TempDir(), "state")
		var output bytes.Buffer
		if result := run(t.Context(), []string{"--run", "--state-dir", dir, "--expected-endpoint", "127.0.0.1:47000"}, input, &output, io.Discard); result != 1 {
			t.Fatalf("code=%d", result)
		}
		input.Close()
		if strings.Contains(output.String(), "SECRET") || strings.Contains(output.String(), dir) {
			t.Fatal("input leaked")
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid input created state")
		}
	}
}

func TestPinnedLoopbackPairingAndReconnect(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	op, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := listener.Addr().String()
	store, err := trust.Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	token, err := store.Issue(ctx, time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	embedded := hub.NewEmbedded()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hubDone := make(chan error, 1)
	go func() {
		hubDone <- hub.NewServer(hub.Options{Local: op, Embedded: embedded, Logger: logger}).Serve(ctx, listener)
	}()
	_, acceptor, err := hubtransport.New(hubtransport.Options{Local: op, Embedded: embedded, Wrap: func(ctx context.Context, conn net.Conn, initiator bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
		return secure.WrapStream(ctx, conn, secure.Opts{Local: op, IsClient: initiator, Expect: expect, Path: path})
	}})
	if err != nil {
		t.Fatal(err)
	}
	workerDone := make(chan struct{})
	var sessions, consumed atomic.Uint64
	go func() {
		defer close(workerDone)
		for {
			ts, err := acceptor.Accept(ctx)
			if err != nil {
				return
			}
			s, err := session.Run(ctx, ts, session.Options{Role: session.RoleOperator, AllowLinkTest: true, Logger: logger,
				Trusted: func(peer identity.Fingerprint) bool { ok, _ := store.Trusted(ctx, peer); return ok },
				AcceptToken: func(peer identity.Fingerprint, token [16]byte) bool {
					consumed.Add(1)
					ok, _ := store.Consume(ctx, peer, token)
					return ok
				},
			})
			if err != nil {
				_ = ts.Close(err)
				return
			}
			sessions.Add(1)
			<-s.Done()
			_ = ts.Close(context.Canceled)
		}
	}()
	t.Cleanup(func() { cancel(); _ = acceptor.Close(); <-workerDone; <-hubDone })
	peer := transport.PeerInfo{ID: op.Fingerprint(), Endpoints: []transport.Endpoint{{Kind: "hub", Addr: endpoint, Hub: op.Fingerprint()}}}
	encoded, err := (invite.Invite{Version: 1, Peer: peer, Token: token, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "invite")
	if err := os.WriteFile(file, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	var output bytes.Buffer
	code := run(ctx, []string{"--run", "--hold", "0", "--expected-endpoint", endpoint, "--state-dir", filepath.Join(t.TempDir(), "state")}, input, &output, io.Discard)
	var r report
	if err := json.Unmarshal(output.Bytes(), &r); err != nil {
		t.Fatal(err)
	}
	if code != 0 || !r.Success || !r.Cleanup || r.RejectedFlows != 0 || len(r.Phases) != 2 || sessions.Load() != 2 || consumed.Load() != 1 {
		t.Fatalf("result=%s sessions=%d consumed=%d", output.String(), sessions.Load(), consumed.Load())
	}
	for _, p := range r.Phases {
		if p.Code != "ok" || p.Samples != 10 || p.Sent != 1<<20 || p.Received != 1<<20 {
			t.Fatalf("phase: %+v", p)
		}
	}
	if strings.Contains(output.String(), encoded) || strings.Contains(output.String(), endpoint) || strings.Contains(output.String(), op.Fingerprint().String()) {
		t.Fatal("report leaked endpoint or identity")
	}
}

func TestCancellationClosesHandshakeSocket(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		close(accepted)
		_, _ = io.Copy(io.Discard, conn)
	}()
	peer := transport.PeerInfo{ID: identity.Fingerprint{1}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: listener.Addr().String(), Hub: identity.Fingerprint{1}}}}
	encoded, err := (invite.Invite{Version: 1, Peer: peer, Token: [16]byte{1}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(encoded), 0o600); err != nil {
		t.Fatal(err)
	}
	input, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan report, 1)
	go func() { done <- probe(ctx, input, listener.Addr().String(), filepath.Join(t.TempDir(), "state"), 0) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake did not start")
	}
	cancel()
	select {
	case r := <-done:
		if r.Success || !r.Canceled || !r.Cleanup {
			t.Fatalf("canceled probe: %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe cancellation did not join")
	}
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake socket remained open")
	}
}
