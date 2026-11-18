package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"remote-enet/internal/hsfz"
)

func signer(t *testing.T) (ssh.Signer, []byte) {
	t.Helper()
	_, key, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	s, e := ssh.NewSignerFromKey(key)
	if e != nil {
		t.Fatal(e)
	}
	der, e := x509.MarshalPKCS8PrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	return s, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
func fixture(t *testing.T, mode string) (options, string) {
	t.Helper()
	host, _ := signer(t)
	user, private := signer(t)
	dir := t.TempDir()
	o := options{run: true, target: "192.0.2.3", user: "fixture", identity: filepath.Join(dir, "identity"), hostKey: filepath.Join(dir, "host.pub"), payload: 1048576, timeout: 3 * time.Second}
	if e := os.WriteFile(o.identity, private, 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(o.hostKey, ssh.MarshalAuthorizedKey(host.PublicKey()), 0600); e != nil {
		t.Fatal(e)
	}
	gateway, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	o.gateway = gateway.Addr().String()
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var connections []net.Conn
	track := func(c net.Conn) { mu.Lock(); connections = append(connections, c); mu.Unlock() }
	wg.Go(func() {
		for {
			c, e := gateway.Accept()
			if e != nil {
				return
			}
			track(c)
			wg.Go(func() {
				defer c.Close()
				if _, e := hsfz.ReadFrame(c, 4096); e != nil {
					return
				}
				_ = hsfz.WriteFrame(c, hsfz.Frame{Control: hsfz.ControlDiag, Body: hsfz.DiagBody{Src: 0x63, Dst: 0xf4, Payload: []byte{0x62, 0x17, 0x2a, 0, 192, 0, 2, 3, 255, 255, 255, 0, 0, 0, 0, 0}}.Marshal()})
			})
		}
	})
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
		if !bytes.Equal(k.Marshal(), user.PublicKey().Marshal()) {
			return nil, fmt.Errorf("unauthorized")
		}
		return nil, nil
	}}
	config.AddHostKey(host)
	wg.Go(func() {
		for {
			c, e := listener.Accept()
			if e != nil {
				return
			}
			track(c)
			wg.Go(func() {
				defer c.Close()
				sc, chans, req, e := ssh.NewServerConn(c, config)
				if e != nil {
					return
				}
				defer sc.Close()
				wg.Go(func() { ssh.DiscardRequests(req) })
				for ch := range chans {
					if ch.ChannelType() != "session" {
						_ = ch.Reject(ssh.UnknownChannelType, "unsupported")
						continue
					}
					channel, requests, e := ch.Accept()
					if e != nil {
						return
					}
					wg.Go(func() {
						defer channel.Close()
						for req := range requests {
							if req.Type != "exec" {
								_ = req.Reply(false, nil)
								continue
							}
							var command struct{ Command string }
							if ssh.Unmarshal(req.Payload, &command) != nil {
								return
							}
							_ = req.Reply(true, nil)
							switch {
							case command.Command == "read-only-marker":
								if mode == "timeout" {
									_ = sc.Wait()
									return
								}
								_, _ = io.WriteString(channel, "remote-enet-mgu-fixture")
								if mode == "marker-newline" {
									_, _ = io.WriteString(channel, "\n")
								}
								if mode == "marker-wrong" {
									_, _ = io.WriteString(channel, "wrong")
								}
							case strings.HasPrefix(command.Command, "read-only-bulk "):
								var n int64
								_, _ = fmt.Sscanf(command.Command, "read-only-bulk %d", &n)
								if mode == "partial" {
									n /= 2
								}
								_, _ = io.CopyN(channel, zeroReader{}, n)
							case strings.HasPrefix(command.Command, "read-only-upload "):
								var n int64
								_, _ = fmt.Sscanf(command.Command, "read-only-upload %d", &n)
								actual, _ := io.Copy(io.Discard, channel)
								if mode == "receipt" {
									_, _ = io.WriteString(channel, `{"bytes":1,"sha256":"bad","secret":true}`)
								} else {
									_ = json.NewEncoder(channel).Encode(map[string]any{"bytes": actual, "sha256": zeroHash(n)})
								}
							default:
								return
							}
							_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					})
				}
			})
		}
	})
	t.Cleanup(func() {
		_ = gateway.Close()
		_ = listener.Close()
		mu.Lock()
		for _, c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return o, listener.Addr().String()
}
func TestProbe(t *testing.T) {
	for _, mode := range []string{"ok", "8m", "marker-newline", "marker-wrong", "partial", "receipt", "timeout", "pin", "identity", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			o, address := fixture(t, mode)
			if mode == "8m" {
				o.payload = 8388608
			}
			if mode == "pin" {
				s, _ := signer(t)
				_ = os.WriteFile(o.hostKey, ssh.MarshalAuthorizedKey(s.PublicKey()), 0600)
			}
			if mode == "identity" {
				_, key := signer(t)
				_ = os.WriteFile(o.identity, key, 0600)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				cancel()
			}
			if mode == "timeout" {
				o.timeout = time.Second
			}
			r := probe(ctx, o, address)
			if r.Success != (mode == "ok" || mode == "8m") {
				t.Fatalf("unexpected report: %+v", r)
			}
			if !r.Cleanup {
				t.Fatal("cleanup not joined")
			}
			if mode == "timeout" && !r.Timeout {
				t.Fatal("missing timeout")
			}
			if mode == "cancel" && !r.Cancelled {
				t.Fatal("missing cancellation")
			}
			if mode == "ok" {
				for _, p := range r.Phases {
					if (p.Stage == "download" || p.Stage == "upload") && (p.Bytes != o.payload || p.SHA256 != zeroHash(o.payload)) {
						t.Fatalf("bad result: %+v", p)
					}
				}
			}
		})
	}
}
func TestDefaultNoSideEffects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report")
	for _, args := range [][]string{nil, {"--help"}, {"--version"}, {"--identity-file", filepath.Join(dir, "missing"), "--host-key-file", "missing", "--gateway", "127.0.0.1:1", "--report", path}} {
		var out bytes.Buffer
		if code := cli(context.Background(), args, &out); code != 0 {
			t.Fatalf("code %d: %s", code, out.String())
		}
	}
	entries, e := os.ReadDir(dir)
	if e != nil || len(entries) != 0 {
		t.Fatal("default wrote files")
	}
}
func TestValidation(t *testing.T) {
	for _, target := range []string{"example.org", "::ffff:127.0.0.1", "0.0.0.0", "224.0.0.1", "8.8.8.8"} {
		o := options{target: target, gateway: "127.0.0.1:1", payload: 1048576, timeout: time.Second, user: "u", identity: "missing", hostKey: "missing"}
		r := probe(context.Background(), o, "")
		if len(r.Phases) != 1 || r.Phases[0].Code != "invalid_options" {
			t.Fatalf("accepted %s", target)
		}
	}
}
func TestReport(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.json")
	r := baseReport()
	if e := writeReport(path, r); e != nil {
		t.Fatal(e)
	}
	s, e := os.Stat(path)
	if e != nil {
		t.Fatal(e)
	}
	// Windows access control comes from the destination directory's ACL, not Unix mode bits.
	if runtime.GOOS != "windows" && s.Mode().Perm() != 0600 {
		t.Fatal("permissions")
	}
	if e := os.Symlink(path, filepath.Join(dir, "link")); e == nil {
		if writeReport(filepath.Join(dir, "link"), r) == nil {
			t.Fatal("accepted symlink")
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, s := range entries {
		if strings.HasPrefix(s.Name(), ".labprobe-") {
			t.Fatal("temporary leak")
		}
	}
}

func TestReportThroughDirectoryAlias(t *testing.T) {
	parent := t.TempDir()
	directory := filepath.Join(parent, "reports")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(directory, alias); err != nil {
		t.Skipf("directory symlink unavailable: %v", err)
	}
	r := baseReport()
	if err := writeReport(filepath.Join(alias, "report.json"), r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "report.json"))
	if err != nil || !json.Valid(data) {
		t.Fatalf("directory alias did not write a complete report: %v", err)
	}
	assertNoReportTemporary(t, directory)
}

func TestReportDirectorySpelling(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "MixedCase")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	spelling := strings.ToUpper(directory)
	want, err := os.Stat(directory)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.Stat(spelling)
	if err != nil || !os.SameFile(want, got) {
		t.Skip("filesystem does not provide a case-insensitive directory alias")
	}
	if err := writeReport(filepath.Join(spelling, "report.json"), baseReport()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "report.json"))
	if err != nil || !json.Valid(data) {
		t.Fatalf("directory spelling did not write a complete report: %v", err)
	}
	assertNoReportTemporary(t, directory)
}

func TestReportReplacesExistingFile(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "report.json")
	if err := os.WriteFile(path, []byte("old report"), 0666); err != nil {
		t.Fatal(err)
	}
	for _, generation := range []bool{false, true} {
		r := baseReport()
		r.Success = generation
		if err := writeReport(path, r); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(path)
		var got report
		if err != nil || json.Unmarshal(data, &got) != nil || got.Success != generation {
			t.Fatalf("existing report was not replaced completely: %v", err)
		}
	}
	assertNoReportTemporary(t, directory)
}

func TestReportRejectsUnsafeDestination(t *testing.T) {
	directory := t.TempDir()
	original := filepath.Join(directory, "original")
	contents := []byte("keep existing data")
	if err := os.WriteFile(original, contents, 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"link", "dangling"} {
		t.Run(name, func(t *testing.T) {
			target := original
			if name == "dangling" {
				target = filepath.Join(directory, "missing")
			}
			path := filepath.Join(directory, name)
			if err := os.Symlink(target, path); err != nil {
				t.Skipf("file symlink unavailable: %v", err)
			}
			if err := writeReport(path, baseReport()); err == nil {
				t.Fatal("accepted a symlink destination")
			}
		})
	}
	if err := writeReport(directory, baseReport()); err == nil {
		t.Fatal("accepted a directory destination")
	}
	data, err := os.ReadFile(original)
	if err != nil || !bytes.Equal(data, contents) {
		t.Fatal("rejected destination changed existing data")
	}
	assertNoReportTemporary(t, directory)
}

func assertNoReportTemporary(t *testing.T, directory string) {
	t.Helper()
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".labprobe-") {
			t.Fatal("temporary report leaked")
		}
	}
}

func TestReceiptStrict(t *testing.T) {
	sum := zeroHash(1048576)
	for _, data := range []string{`{"bytes":1048576,"bytes":1048576,"sha256":"` + sum + `"}`, `{"bytes":1048576,"sha256":"` + sum + `"} {}`, `{"bytes":1048576}`, `{"bytes":1048576,"sha256":"` + sum + `","extra":0}`} {
		if validReceipt([]byte(data), 1048576, sum) {
			t.Fatalf("accepted malformed receipt %s", data)
		}
	}
}

func TestHandshakeCancellation(t *testing.T) {
	o, _ := fixture(t, "ok")
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	accepted := make(chan struct{})
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		close(accepted)
		_, _ = io.Copy(io.Discard, c)
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan report, 1)
	go func() { done <- probe(ctx, o, listener.Addr().String()) }()
	select {
	case <-accepted:
	case <-time.After(3 * time.Second):
		t.Fatal("handshake not started")
	}
	cancel()
	select {
	case r := <-done:
		if r.Success || !r.Cancelled || !r.Cleanup {
			t.Fatalf("bad report %+v", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("probe did not join")
	}
	select {
	case <-joined:
	case <-time.After(3 * time.Second):
		t.Fatal("connection not closed")
	}
}
func TestTargetMismatch(t *testing.T) {
	o, address := fixture(t, "ok")
	o.target = "192.0.2.4"
	r := probe(context.Background(), o, address)
	if r.Success || r.Phases[len(r.Phases)-1].Code != "target_mismatch" {
		t.Fatalf("bad report %+v", r)
	}
}
