package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"remote-enet/internal/mgu"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

const version = "1"

type options struct {
	run                                              bool
	gateway, target, user, identity, hostKey, report string
	payload                                          int64
	timeout                                          time.Duration
}
type phase struct {
	Stage        string `json:"stage"`
	Code         string `json:"code"`
	Milliseconds int64  `json:"duration_ms"`
	Bytes        int64  `json:"bytes"`
	SHA256       string `json:"sha256,omitempty"`
}
type report struct {
	Schema         string  `json:"schema"`
	Version        string  `json:"version"`
	Platform       string  `json:"platform"`
	Success        bool    `json:"success"`
	DryRun         bool    `json:"dry_run"`
	Cancelled      bool    `json:"cancelled"`
	Timeout        bool    `json:"timeout"`
	Cleanup        bool    `json:"cleanup"`
	ExpectedBytes  int64   `json:"expected_bytes"`
	ExpectedSHA256 string  `json:"expected_sha256,omitempty"`
	Phases         []phase `json:"phases"`
}

func baseReport() report {
	return report{Schema: "labprobe/v1", Version: version, Platform: runtime.GOOS + "/" + runtime.GOARCH, Phases: []phase{}}
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	code := cli(ctx, os.Args[1:], os.Stdout)
	stop()
	os.Exit(code)
}
func cli(ctx context.Context, args []string, out io.Writer) int {
	o := options{}
	f := flag.NewFlagSet("renet-labprobe", flag.ContinueOnError)
	f.SetOutput(io.Discard)
	f.BoolVar(&o.run, "run", false, "execute the isolated fixture probe")
	v := f.Bool("version", false, "show version")
	f.StringVar(&o.gateway, "gateway", "", "literal fixture IPv4:port")
	f.StringVar(&o.target, "target", "", "literal fixture IPv4")
	f.StringVar(&o.user, "ssh-user", "", "fixture user")
	f.StringVar(&o.identity, "identity-file", "", "fixture private key")
	f.StringVar(&o.hostKey, "host-key-file", "", "fixture authorized-format host public key")
	f.StringVar(&o.report, "report", "", "atomic JSON report path")
	f.Int64Var(&o.payload, "payload-bytes", 1048576, "1048576 or 8388608")
	f.DurationVar(&o.timeout, "timeout", 30*time.Second, "total budget, 1s..120s")
	err := f.Parse(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(out, "renet-labprobe: default dry-run; --run --gateway IPv4:port --target IPv4 --ssh-user USER --identity-file FILE --host-key-file FILE [--payload-bytes 1048576|8388608] [--timeout 30s] [--report FILE]")
		return 0
	}
	if err == nil && *v {
		fmt.Fprintln(out, "renet-labprobe "+version)
		return 0
	}
	r := baseReport()
	if err != nil || f.NArg() != 0 {
		r.Phases = append(r.Phases, phase{Stage: "input", Code: "invalid_flags"})
		_ = json.NewEncoder(out).Encode(r)
		return 2
	}
	if !o.run {
		r.DryRun = true
		r.Cleanup = true
		_ = json.NewEncoder(out).Encode(r)
		return 0
	}
	r = probe(ctx, o, "")
	if o.report != "" {
		if err := writeReport(o.report, r); err != nil {
			r.Success = false
			r.Phases = append(r.Phases, phase{Stage: "report", Code: "write_failed"})
		}
	}
	_ = json.NewEncoder(out).Encode(r)
	if !r.Success {
		return 1
	}
	return 0
}
func allowed(a netip.Addr) bool {
	if !a.Is4() || a.IsUnspecified() || a.IsMulticast() {
		return false
	}
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || netip.MustParsePrefix("192.0.2.0/24").Contains(a) || netip.MustParsePrefix("198.51.100.0/24").Contains(a) || netip.MustParsePrefix("203.0.113.0/24").Contains(a) || netip.MustParsePrefix("198.18.0.0/15").Contains(a)
}

type directOpener struct{ gateway netip.AddrPort }

func (d directOpener) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	if h.Kind != "tcp" || h.Target != d.gateway.String() {
		return nil, errors.New("unexpected destination")
	}
	c, e := (&net.Dialer{}).DialContext(ctx, "tcp4", h.Target)
	if e != nil {
		return nil, e
	}
	return c.(*net.TCPConn), nil
}
func zeroHash(n int64) string {
	h := sha256.New()
	_, _ = io.CopyN(h, zeroReader{}, n)
	return hex.EncodeToString(h.Sum(nil))
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func probe(parent context.Context, o options, sshAddress string) (r report) {
	r = baseReport()
	r.ExpectedBytes = o.payload
	cleanupOK := true
	stage := "input"
	start := time.Now()
	fail := func(code string) {
		r.Phases = append(r.Phases, phase{Stage: stage, Code: code, Milliseconds: time.Since(start).Milliseconds()})
	}
	gateway, e := netip.ParseAddrPort(o.gateway)
	target, te := netip.ParseAddr(o.target)
	if e != nil || te != nil || !allowed(gateway.Addr()) || gateway.Port() == 0 || !allowed(target) || (o.payload != 1048576 && o.payload != 8388608) || o.timeout < time.Second || o.timeout > 120*time.Second || o.user == "" || strings.ContainsAny(o.user, "\x00\r\n") || o.identity == "" || o.hostKey == "" {
		fail("invalid_options")
		r.Cleanup = true
		return
	}
	ctx, cancel := context.WithTimeout(parent, o.timeout)
	defer cancel()
	r.ExpectedSHA256 = zeroHash(o.payload)
	defer func() {
		r.Cancelled = errors.Is(ctx.Err(), context.Canceled)
		deadline, _ := ctx.Deadline()
		r.Timeout = r.Timeout || errors.Is(ctx.Err(), context.DeadlineExceeded) || !time.Now().Before(deadline)
		if ctx.Err() != nil || r.Timeout {
			r.Success = false
			if len(r.Phases) > 0 && r.Phases[len(r.Phases)-1].Code != "close_failed" {
				code := "cancelled"
				if r.Timeout {
					code = "timeout"
				}
				r.Phases[len(r.Phases)-1].Code = code
			}
		}
		r.Cleanup = cleanupOK
	}()
	next := func(s string) { stage = s; start = time.Now() }
	ok := func() { fail("ok") }
	next("keys")
	key, e := readKey(o.identity)
	if e != nil {
		fail("identity_invalid")
		return
	}
	signer, e := ssh.ParsePrivateKey(key)
	clear(key)
	if e != nil {
		fail("identity_invalid")
		return
	}
	host, e := readKey(o.hostKey)
	if e != nil {
		fail("host_key_invalid")
		return
	}
	pinned, _, _, rest, e := ssh.ParseAuthorizedKey(host)
	if e != nil || len(bytes.TrimSpace(rest)) != 0 {
		fail("host_key_invalid")
		return
	}
	ok()
	next("discover")
	info, e := mgu.Discover(ctx, directOpener{gateway}, gateway, mgu.Options{Timeout: min(o.timeout, 30*time.Second)})
	if e != nil {
		if errors.Is(e, context.DeadlineExceeded) {
			r.Timeout = true
			fail("timeout")
		} else {
			fail("query_failed")
		}
		return
	}
	if info.Address != target {
		fail("target_mismatch")
		return
	}
	ok()
	next("ssh")
	if sshAddress == "" {
		sshAddress = net.JoinHostPort(target.String(), "22")
	}
	conn, e := (&net.Dialer{}).DialContext(ctx, "tcp4", sshAddress)
	if e != nil {
		fail("connect_failed")
		return
	}
	closeChecked := func(err error) {
		if err != nil && !errors.Is(err, net.ErrClosed) {
			cleanupOK = false
			r.Success = false
			r.Phases = append(r.Phases, phase{Stage: "cleanup", Code: "close_failed"})
		}
	}
	joined := make(chan struct{})
	var callbackCloseErr error
	stop := context.AfterFunc(ctx, func() { defer close(joined); callbackCloseErr = conn.Close() })
	defer func() {
		closeChecked(conn.Close())
		if !stop() {
			<-joined
			closeChecked(callbackCloseErr)
		}
	}()
	if deadline, yes := ctx.Deadline(); yes {
		_ = conn.SetDeadline(deadline)
	}
	config := &ssh.ClientConfig{User: o.user, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
		if !bytes.Equal(k.Marshal(), pinned.Marshal()) {
			return errors.New("pin mismatch")
		}
		return nil
	}}
	cc, ch, req, e := ssh.NewClientConn(conn, sshAddress, config)
	if e != nil {
		fail("handshake_failed")
		return
	}
	client := ssh.NewClient(cc, ch, req)
	defer func() { closeChecked(client.Close()); _ = client.Wait() }()
	ok()
	next("marker")
	var marker bytes.Buffer
	if e = command(client, "read-only-marker", nil, &limitedWriter{w: &marker, n: 64}); e != nil || marker.String() != "remote-enet-mgu-fixture" {
		fail("marker_invalid")
		return
	}
	ok()
	next("download")
	h := sha256.New()
	counter := &limitedWriter{w: h, n: o.payload + 1}
	e = command(client, fmt.Sprintf("read-only-bulk %d", o.payload), nil, counter)
	sum := hex.EncodeToString(h.Sum(nil))
	r.Phases = append(r.Phases, phase{Stage: stage, Code: "invalid_payload", Milliseconds: time.Since(start).Milliseconds(), Bytes: counter.written, SHA256: sum})
	if e != nil || counter.written != o.payload || sum != r.ExpectedSHA256 {
		return
	}
	r.Phases[len(r.Phases)-1].Code = "ok"
	next("upload")
	var receipt bytes.Buffer
	sent := &countReader{r: io.LimitReader(zeroReader{}, o.payload), h: sha256.New()}
	e = command(client, fmt.Sprintf("read-only-upload %d", o.payload), sent, &limitedWriter{w: &receipt, n: 1024})
	uploadSum := hex.EncodeToString(sent.h.Sum(nil))
	p := phase{Stage: stage, Code: "invalid_receipt", Milliseconds: time.Since(start).Milliseconds(), Bytes: sent.n, SHA256: uploadSum}
	defer func() { r.Phases = append(r.Phases, p) }()
	if e != nil || !validReceipt(receipt.Bytes(), o.payload, r.ExpectedSHA256) || sent.n != o.payload || uploadSum != r.ExpectedSHA256 {
		return
	}
	p.Code = "ok"
	r.Success = true
	return
}

type countReader struct {
	r io.Reader
	h hash.Hash
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, e := c.r.Read(p)
	c.n += int64(n)
	_, _ = c.h.Write(p[:n])
	return n, e
}

type limitedWriter struct {
	w          io.Writer
	n, written int64
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.n {
		return 0, errors.New("output limit")
	}
	n, e := w.w.Write(p)
	w.n -= int64(n)
	w.written += int64(n)
	return n, e
}
func command(c *ssh.Client, name string, in io.Reader, out io.Writer) (err error) {
	s, e := c.NewSession()
	if e != nil {
		return e
	}
	defer func() {
		closeErr := s.Close()
		if err == nil && closeErr != nil && !errors.Is(closeErr, io.EOF) && !errors.Is(closeErr, net.ErrClosed) {
			err = closeErr
		}
	}()
	s.Stdin = in
	s.Stdout = out
	s.Stderr = &limitedWriter{w: io.Discard, n: 4096}
	return s.Run(name)
}
func writeReport(path string, r report) error {
	if s, e := os.Lstat(path); e == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	// Directory aliases are normal on macOS and Windows. Hold the directory
	// handle so temporary files and replacement stay in that same directory.
	dir, e := os.OpenRoot(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer dir.Close()
	destination := filepath.Base(path)
	if s, e := dir.Lstat(destination); e == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	var f *os.File
	var name string
	for range 10 {
		name = ".labprobe-" + rand.Text()
		f, e = dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if !errors.Is(e, os.ErrExist) {
			break
		}
	}
	if e != nil {
		return e
	}
	defer dir.Remove(name)
	defer f.Close()
	if e = f.Chmod(0600); e != nil {
		return e
	}
	if e = json.NewEncoder(f).Encode(r); e != nil {
		return e
	}
	if e = f.Sync(); e != nil {
		return e
	}
	if e = f.Close(); e != nil {
		return e
	}
	if s, e := dir.Lstat(destination); e == nil {
		if !s.Mode().IsRegular() {
			return errors.New("unsafe destination")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	return dir.Rename(name, destination)
}

func validReceipt(data []byte, n int64, sum string) bool {
	d := json.NewDecoder(bytes.NewReader(data))
	token, e := d.Token()
	if e != nil || token != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	var count int64
	var hash string
	for d.More() {
		token, e = d.Token()
		if e != nil {
			return false
		}
		key, ok := token.(string)
		if !ok || seen[key] {
			return false
		}
		seen[key] = true
		switch key {
		case "bytes":
			if d.Decode(&count) != nil {
				return false
			}
		case "sha256":
			if d.Decode(&hash) != nil {
				return false
			}
		default:
			return false
		}
	}
	token, e = d.Token()
	return e == nil && token == json.Delim('}') && d.Decode(new(any)) == io.EOF && len(seen) == 2 && count == n && hash == sum
}
