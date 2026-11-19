package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/hub"
	"remote-enet/internal/hubauth"
	"remote-enet/internal/identity"
	"remote-enet/internal/ticket"
)

type fixture struct {
	path      string
	cfg       hubauth.Config
	local, op *identity.Local
	raw       []byte
}

func writeFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func writeConfig(t *testing.T, path string, c hubauth.Config) {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, b)
}

func setup(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	var out bytes.Buffer
	if err := run(t.Context(), []string{"init", "--identity-dir", dir}, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	local, err := identity.LoadExisting(dir)
	if err != nil || strings.TrimSpace(out.String()) != local.Fingerprint().String() {
		t.Fatalf("init: %v %s", err, &out)
	}
	op, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	pub, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, []byte(fmt.Sprintf(`{"version":1,"mode":"selfhost","identity_dir":%q,"listen":"127.0.0.1:443","issuers":[{"name":"shop","operator":%q,"public_key":%q}]}`, dir, op.Fingerprint().String(), base64.RawURLEncoding.EncodeToString(pub))))
	cfg, err := hubauth.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the empty arrays explicit when round-tripping the strict JSON format.
	cfg.Deny.Devices = []string{}
	cfg.Deny.JTIs = []string{}
	now := time.Now()
	raw, err := ticket.Sign(key, ticket.Claims{Role: uint8(hub.RoleWait), Hub: local.Fingerprint(), Device: op.Fingerprint(), IssuedAt: now, NotBefore: now, Expires: now.Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	return fixture{path, cfg, local, op, raw}
}

func TestCommands(t *testing.T) {
	f := setup(t)
	var out bytes.Buffer
	call := func(args ...string) error { out.Reset(); return run(t.Context(), args, &out, io.Discard) }
	if err := call("init", "--identity-dir", f.cfg.IdentityDir); !errors.Is(err, os.ErrExist) {
		t.Fatal(err)
	}
	if err := call("fingerprint", "--identity-dir", f.cfg.IdentityDir); err != nil || strings.TrimSpace(out.String()) != f.local.Fingerprint().String() {
		t.Fatalf("fingerprint: %v %s", err, &out)
	}
	kid, _, err := ticket.Inspect(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := call("check", "--config", f.path); err != nil || !strings.Contains(out.String(), fmt.Sprintf("name=%q operator=%s kid=%x\npairing=disabled", "shop", f.op.Fingerprint().Short(), kid)) {
		t.Fatalf("check: %v %s", err, &out)
	}
	file := filepath.Join(t.TempDir(), "ticket")
	encoded := base64.RawURLEncoding.EncodeToString(f.raw)
	writeFile(t, file, []byte(" \n"+encoded+"\n"))
	args := []string{"ticket", "inspect", "--config", f.path, "--ticket-file", file}
	if err := call(args...); err != nil || !strings.Contains(out.String(), "Skipped A7") || !strings.Contains(out.String(), "Passed") || strings.Contains(out.String(), encoded) {
		t.Fatalf("inspect: %v", err)
	}
	if err := call(append(args, "--device", f.op.Fingerprint().String())...); err != nil || strings.Contains(out.String(), "Skipped A7") {
		t.Fatalf("inspect device: %v", err)
	}
	if err := call(append(args, "--device", f.local.Fingerprint().String())...); err == nil || !strings.Contains(err.Error(), "A7") {
		t.Fatalf("wrong device: %v", err)
	}
	f.cfg.IdentityDir = filepath.Join(t.TempDir(), "missing")
	writeConfig(t, f.path, f.cfg)
	if err := call(append(args, "--hub-fp", f.local.Fingerprint().String())...); err != nil || !strings.Contains(out.String(), "Passed") {
		t.Fatalf("offline inspect without device key: %v", err)
	}
	if err := call(args...); err == nil || !strings.Contains(err.Error(), "identity_dir") {
		t.Fatalf("inspect without fingerprint or identity: %v", err)
	}
	for _, cmd := range []string{"serve", "check"} {
		if err := call(cmd, "--config", f.path); err == nil || !strings.Contains(err.Error(), "identity_dir") {
			t.Fatalf("%s missing identity: %v", cmd, err)
		}
	}
}

func TestServeReload(t *testing.T) {
	f := setup(t)
	fp := f.local.Fingerprint()
	policy, err := hubauth.NewPolicy(f.cfg, fp)
	if err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	server := hub.NewServer(hub.Options{Local: f.local, Policy: policy, Logger: logger})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	var banner bytes.Buffer
	go func() { done <- serveListener(ctx, f.path, f.cfg, fp, server, ln, nil, &banner, logger) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("shutdown timed out")
		}
		if !strings.Contains(banner.String(), fp.String()) {
			t.Error("missing startup fingerprint")
		}
		s := logs.String()
		if !strings.Contains(s, "closed_waits=1") || !strings.Contains(s, "A11: revoked") || strings.Contains(s, "23980716") || strings.Contains(s, string(f.raw)) || strings.Contains(s, base64.RawURLEncoding.EncodeToString(f.raw)) || strings.Contains(s, f.op.Fingerprint().String()) {
			t.Error("unexpected reload or admission logs")
		}
	}()
	req := hub.Request{Role: hub.RoleWait, Token: f.raw}
	current := f.cfg
	for _, field := range []string{"invalid", "listen", "identity_dir", "setup_limits"} {
		next := f.cfg
		switch field {
		case "invalid":
			writeFile(t, f.path, []byte("{"))
		case "listen":
			next.Listen = ":444"
		case "identity_dir":
			next.IdentityDir = t.TempDir()
		case "setup_limits":
			next.SetupLimits.Concurrent++
		}
		if field != "invalid" {
			writeConfig(t, f.path, next)
		}
		err := reload(f.path, &current, fp, server, logger)
		if err == nil || field != "invalid" && !strings.Contains(err.Error(), field) || !reflect.DeepEqual(current, f.cfg) {
			t.Fatalf("reject %s: %v", field, err)
		}
	}
	connect := func() (net.Conn, error) { return hub.Connect(ctx, ln.Addr().String(), f.op, fp, req) }
	if _, err := hub.Connect(ctx, ln.Addr().String(), f.op, fp, hub.Request{Role: hub.RoleWait, Token: []byte("23980716")}); !errors.Is(err, hub.ErrUnauthorized) {
		t.Fatalf("invalid token: %v", err)
	}
	conn, err := connect()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	next := f.cfg
	next.Deny.Devices = []string{f.op.Fingerprint().String()}
	writeConfig(t, f.path, next)
	if err := reload(f.path, &current, fp, server, logger); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	var timeout net.Error
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
		t.Fatalf("wait still open: %v", err)
	}
	if _, err := connect(); !errors.Is(err, hub.ErrUnauthorized) {
		t.Fatalf("revoked wait: %v", err)
	}
}

func TestInspectRejectsTamperedClaimsBeforeDisplay(t *testing.T) {
	f := setup(t)
	kid, c, err := ticket.Inspect(f.raw)
	if err != nil {
		t.Fatal(err)
	}
	f.raw[len(f.raw)-1] ^= 1
	file := filepath.Join(t.TempDir(), "ticket")
	writeFile(t, file, []byte(base64.RawURLEncoding.EncodeToString(f.raw)))
	var out bytes.Buffer
	err = inspect(&out, f.cfg, f.local.Fingerprint(), file, "")
	if err == nil || !strings.Contains(err.Error(), "A4") || !strings.Contains(err.Error(), fmt.Sprintf("unverified kid=%x", kid)) {
		t.Fatalf("diagnostic: %v", err)
	}
	if out.Len() != 0 || strings.Contains(err.Error(), c.Device.Short()) || strings.Contains(err.Error(), base64.RawURLEncoding.EncodeToString(c.JTI[:])) {
		t.Fatal("displayed unauthenticated claims")
	}
}
