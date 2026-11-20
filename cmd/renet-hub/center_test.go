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
	"net"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/hub"
	"remote-enet/internal/hubcenter"
	"remote-enet/internal/identity"
	"remote-enet/internal/testutil/fakecenter"
	"remote-enet/internal/ticket"
)

type centerFixture struct {
	fixture
	fc       *fakecenter.Center
	addr, id string
	config   map[string]any
}

func (f centerFixture) save(t *testing.T) {
	t.Helper()
	data, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, f.path, data)
}

func setupCenter(t *testing.T) centerFixture {
	t.Helper()
	f := centerFixture{fixture: setup(t), fc: fakecenter.New(t, fakecenter.Options{Entries: 2})}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = ln.Addr().String()
	f.id = f.fc.RegisterNode(fakecenter.NodeConfig{FP: f.local.Fingerprint(), Addr: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port})
	ln.Close()
	f.config = map[string]any{"version": 1, "mode": "center", "identity_dir": f.cfg.IdentityDir, "listen": f.addr, "center": map[string]any{"id": "test-center", "api": f.fc.Entries()}, "node_id": f.id, "ticket_keys": []string{base64.RawURLEncoding.EncodeToString(f.op.Key.Public().(ed25519.PublicKey))}}
	f.save(t)
	original := http.DefaultTransport
	http.DefaultTransport = f.fc.HTTPClient().Transport
	t.Cleanup(func() { http.DefaultTransport = original })
	return f
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCenterCheck(t *testing.T) {
	f := setupCenter(t)
	saved := version
	defer func() { version = saved }()
	base := http.DefaultTransport
	expected := "0.0.0-dev"
	http.DefaultTransport = transportFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" || r.URL.Path != "/v1/meta" || r.Header.Get("Renet-Client") != "hub/"+expected || r.Header.Get("Renet-Signature") != "" || r.Header.Get("Renet-Node-Id") != "" {
			t.Errorf("unexpected metadata request")
		}
		return base.RoundTrip(r)
	})
	var out bytes.Buffer
	args := []string{"check", "--config", f.path}
	if err := run(t.Context(), args, &out, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, field := range append(f.fc.Entries(), "center_id=test-center", "node_id="+f.id, "hub="+f.local.Fingerprint().String(), "kid="+center.KeyID(ticket.KidOf(f.op.Key.Public().(ed25519.PublicKey))).String()) {
		if !strings.Contains(out.String(), field) {
			t.Errorf("missing %q", field)
		}
	}
	for i := range f.fc.Entries() {
		if f.fc.Calls(i, "/v1/meta") != 0 {
			t.Fatal("offline check sent request")
		}
	}
	for _, v := range []string{"dev", "1.2.3"} {
		version = v
		if v != "dev" {
			expected = v
		}
		out.Reset()
		if err := run(t.Context(), append(args, "--online"), &out, io.Discard); err != nil || strings.Count(out.String(), "status=verified") != 2 {
			t.Fatal(err, out.String())
		}
	}
	f.fc.Inject(0, "/v1/meta", fakecenter.Fault{Code: "bad_request"})
	out.Reset()
	if err := run(t.Context(), append(args, "--online"), &out, io.Discard); err == nil || !strings.Contains(out.String(), "status=unreachable") || !strings.Contains(out.String(), "status=verified") {
		t.Fatal(err, out.String())
	}
	f.config["extra"] = true
	f.save(t)
	if err := run(t.Context(), args, &out, io.Discard); err == nil {
		t.Fatal("accepted unknown field")
	}
}

func TestCenterReload(t *testing.T) {
	f := setupCenter(t)
	cfg, err := hubcenter.Load(f.path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := hubcenter.NewPolicy(cfg, f.local.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	node := hubcenter.NewNode(hubcenter.NodeOptions{Config: cfg, Policy: p, Server: hub.NewServer(hub.Options{Policy: p})})
	f.config["ticket_keys"] = []string{base64.RawURLEncoding.EncodeToString(f.fc.TicketKey())}
	f.save(t)
	if err := reloadCenter(f.path, node); err != nil {
		t.Fatal(err)
	}
	want := []ticket.Kid{ticket.KidOf(f.fc.TicketKey())}
	f.config["listen"] = ":443"
	f.config["ticket_keys"] = []string{base64.RawURLEncoding.EncodeToString(cfg.TicketKeys[0])}
	f.save(t)
	if err := reloadCenter(f.path, node); err == nil || !slices.Equal(p.Kids(), want) {
		t.Fatal("incompatible reload changed policy")
	}
	writeFile(t, f.path, []byte("{"))
	if err := reloadCenter(f.path, node); err == nil || !slices.Equal(p.Kids(), want) {
		t.Fatal("invalid reload changed policy")
	}
}

func TestCenterServe(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
	}{{"terminal", 2}, {"superseded", 3}, {"cancel", 0}} {
		t.Run(tc.name, func(t *testing.T) {
			f := setupCenter(t)
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			if tc.code == 2 {
				f.fc.Inject(0, "/v1/node/activate", fakecenter.Fault{Code: "node_unknown"})
			}
			if tc.code == 3 {
				f.fc.Inject(0, "/v1/node/heartbeat", fakecenter.Fault{Code: "node_superseded"})
			}
			var logs bytes.Buffer
			var serveErr error
			done := make(chan struct{})
			go func() {
				defer close(done)
				serveErr = run(ctx, []string{"serve", "--config", f.path, "--debug"}, io.Discard, &logs)
			}()
			defer func() { cancel(); <-done }()
			if tc.code != 2 {
				peer, err := identity.Generate()
				if err != nil {
					t.Fatal(err)
				}
				var id center.NodeID
				if err := id.UnmarshalText([]byte(f.id)); err != nil {
					t.Fatal(err)
				}
				var tokens [][]byte
				var conns []net.Conn
				for i, dev := range []*identity.Local{f.op, peer} {
					role, target := hub.RoleWait, peer.Fingerprint()
					if i == 1 {
						role, target = hub.RoleDial, f.op.Fingerprint()
					}
					now := time.Now()
					claims := ticket.CenterClaims{
						Center: "test-center", Node: id, Hub: f.local.Fingerprint(), Generation: 1,
						Device: dev.Fingerprint(), Peer: target, Role: uint8(role),
						Session: [16]byte{1}, JTI: [16]byte{byte(i + 1)}, IssuedAt: now, NotBefore: now,
					}
					raw, err := ticket.SignCenter(f.op.Key, claims, ticket.CenterIssuePolicy{
						TicketTTL: 120, TicketMaxTTL: 300, ClockSkew: 30, HardDeadline: now.Add(time.Hour),
					})
					if err != nil {
						t.Fatal(err)
					}
					tokens = append(tokens, raw)
					req := hub.Request{Role: role, Target: target, Token: raw}
					deadline := time.Now().Add(4 * time.Second)
					for {
						c, err := hub.Connect(ctx, f.addr, dev, f.local.Fingerprint(), req)
						if err == nil {
							defer c.Close()
							conns = append(conns, c)
							break
						}
						if time.Now().After(deadline) {
							t.Fatal(err)
						}
						time.Sleep(10 * time.Millisecond)
					}
					if _, err := hub.Connect(ctx, f.addr, dev, f.local.Fingerprint(), req); !errors.Is(err, hub.ErrUnauthorized) {
						t.Fatalf("replay: %v", err)
					}
				}
				if tc.code == 0 {
					cancel()
				}
				<-done
				for _, c := range conns {
					if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
						t.Fatal(err)
					}
					var timeout net.Error
					if _, err := c.Read(make([]byte, 1)); err == nil || errors.As(err, &timeout) && timeout.Timeout() {
						t.Fatalf("connection survived shutdown: %v", err)
					}
				}
				for _, raw := range tokens {
					for _, v := range []string{string(raw), base64.RawURLEncoding.EncodeToString(raw), fmt.Sprintf("%x", raw)} {
						if strings.Contains(logs.String(), v) {
							t.Fatal("complete ticket in log")
						}
					}
				}
			} else {
				<-done
			}
			if got := exitCode(serveErr); got != tc.code {
				t.Fatalf("exit %d, want %d: %v", got, tc.code, serveErr)
			}
		})
	}
}
