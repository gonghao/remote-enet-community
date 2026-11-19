package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/ticket"
	"remote-enet/internal/trust"
)

func TestHubCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	opDir, hubDir := t.TempDir(), t.TempDir()
	host, err := identity.LoadOrGenerate(hubDir)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var entry, stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"issuer", "--state-dir", opDir, "--name", "test"}, &entry, &stderr); err != nil {
		t.Fatal(err)
	}
	var issuer hubauth.Issuer
	if err := json.Unmarshal(entry.Bytes(), &issuer); err != nil || issuer.Name != "test" {
		t.Fatal("issuer JSON", err)
	}
	hubArgs := []string{"--hub", ln.Addr().String(), "--hub-fp", host.Fingerprint().String()}
	args := append([]string{"invite", "--state-dir", opDir, "--ttl", "24h"}, hubArgs...)
	if err := run(ctx, args, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	inv, err := invite.Decode(strings.TrimSpace(stdout.String()))
	if err != nil || inv.Version != 2 || inv.Peer.ID.String() != issuer.Operator || len(inv.Peer.Endpoints) != 1 {
		t.Fatal("invalid hub invitation", err)
	}
	ep := inv.Peer.Endpoints[0]
	_, claims, err := ticket.Inspect([]byte(ep.Ticket))
	if err != nil || !claims.Bearer || claims.Hub != host.Fingerprint() || claims.Peer != inv.Peer.ID || !claims.Expires.Equal(inv.ExpiresAt) {
		t.Fatal("invalid bearer claims", err)
	}
	if !strings.Contains(stderr.String(), "jti="+base64.RawURLEncoding.EncodeToString(claims.JTI[:])) || strings.Contains(stderr.String(), "renet1:") {
		t.Fatal("unexpected invite diagnostic")
	}
	store, err := trust.Open(ctx, opDir)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.Consume(ctx, identity.Fingerprint{1}, inv.Token); err != nil || !ok {
		t.Fatal("missing one-time token", err)
	}

	// Feed the exported entry unmodified to the real Hub policy.
	config, err := json.Marshal(map[string]any{"version": 1, "mode": "selfhost", "identity_dir": hubDir, "listen": ep.Addr, "issuers": []json.RawMessage{entry.Bytes()}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "hub.json")
	if err := os.WriteFile(path, config, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := hubauth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := hubauth.NewPolicy(cfg, host.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.NewServer(hub.Options{Local: host, Policy: policy}).Serve(ctx, ln) }()
	defer func() {
		cancel()
		if err := <-hubDone; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	opDone := make(chan error, 1)
	go func() {
		opDone <- run(ctx, append([]string{"operator", "--state-dir", opDir, "--bind", "127.0.0.1", "--exclusive=false", "--share-address", "public.example:47000"}, hubArgs...), io.Discard, io.Discard)
	}()
	defer func() {
		cancel()
		if err := <-opDone; err != nil {
			t.Error(err)
		}
	}()
	client, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	for {
		conn, err := hub.Connect(ctx, ep.Addr, client, ep.Hub, hub.Request{Role: hub.RoleDial, Target: inv.Peer.ID, Token: []byte(ep.Ticket)})
		if err == nil {
			conn.Close()
			break
		}
		if !errors.Is(err, hub.ErrUnknownPeer) {
			t.Fatal("bearer dial", err)
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

func TestHubArgumentConflicts(t *testing.T) {
	pin := (identity.Fingerprint{1}).String()
	for _, args := range [][]string{
		{"operator", "--listen", "", "--hub", "localhost:1", "--hub-fp", pin, "--bind", "127.0.0.1"},
		{"invite", "--endpoint", "localhost:2", "--hub", "localhost:1", "--hub-fp", pin},
		{"invite", "--hub", "localhost:1"}, {"invite", "--hub-fp", pin},
		{"invite", "--hub", "localhost:1", "--hub-fp", pin, "--ttl", "24h1s"},
		{"issuer", "--name", "invalid\nname"},
	} {
		if err := run(t.Context(), append(args, "--state-dir", t.TempDir()), io.Discard, io.Discard); err == nil {
			t.Fatal("accepted conflicting or invalid arguments", args[0])
		}
	}
}
