package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/invite"
	"remote-enet/internal/testutil/fakezgw"
)

type cliLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
	secret string
}

func (l *cliLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *cliLog) snapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.secret != "" {
		return strings.ReplaceAll(l.buffer.String(), l.secret, "[invitation redacted]")
	}
	return l.buffer.String()
}

func (l *cliLog) containsSecret() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.secret != "" && strings.Contains(l.buffer.String(), l.secret)
}

func (l *cliLog) setSecret(secret string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.secret = secret
}

type cliProcess struct {
	cmd     *exec.Cmd
	logs    *cliLog
	done    chan struct{}
	waitErr error
	cursor  int
	forced  bool
}

func cliStart(t *testing.T, ctx context.Context, binary, secret string, args ...string) *cliProcess {
	t.Helper()
	p := &cliProcess{cmd: exec.CommandContext(ctx, binary, args...), logs: &cliLog{secret: secret}, done: make(chan struct{})}
	p.cmd.Stdout = io.Discard
	p.cmd.Stderr = p.logs
	p.cmd.WaitDelay = time.Second
	if err := p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.waitErr = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { p.stop(t) })
	return p
}

func (p *cliProcess) stop(t *testing.T) {
	t.Helper()
	defer func() {
		if p.logs.containsSecret() {
			t.Error("CLI leaked the invitation in its logs")
		}
	}()
	select {
	case <-p.done:
		if p.waitErr != nil && !p.forced {
			t.Errorf("CLI exited unsuccessfully: %v\n%s", p.waitErr, p.logs.snapshot())
		}
		return
	default:
	}
	graceful := p.cmd.Process.Signal(os.Interrupt) == nil
	if !graceful {
		p.forced = true
		_ = p.cmd.Process.Kill()
		if runtime.GOOS == "windows" {
			t.Log("Windows process interruption is unsupported; cleanup uses Kill")
		}
	}
	select {
	case <-p.done:
		if graceful && p.waitErr != nil {
			t.Errorf("CLI failed after interrupt: %v\n%s", p.waitErr, p.logs.snapshot())
		}
	case <-time.After(3 * time.Second):
		p.forced = true
		_ = p.cmd.Process.Kill()
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
			t.Error("CLI process did not exit after Kill")
		}
		t.Errorf("CLI did not exit after interrupt\n%s", p.logs.snapshot())
	}
}

func (p *cliProcess) waitLog(t *testing.T, ctx context.Context, message string) string {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		lines := strings.Split(p.logs.snapshot(), "\n")
		for i := p.cursor; i < len(lines)-1; i++ {
			if strings.Contains(lines[i], `msg="`+message+`"`) {
				p.cursor = i + 1
				return lines[i]
			}
		}
		select {
		case <-p.done:
			t.Fatalf("CLI exited while waiting for %q: %v\n%s", message, p.waitErr, p.logs.snapshot())
		case <-ctx.Done():
			t.Fatalf("waiting for %q: %v\n%s", message, ctx.Err(), p.logs.snapshot())
		case <-ticker.C:
		}
	}
}

func cliField(t *testing.T, line, name string) string {
	t.Helper()
	for _, part := range strings.Fields(line) {
		key, value, ok := strings.Cut(part, "=")
		if ok && key == name {
			return strings.Trim(value, `"`)
		}
	}
	t.Fatalf("missing log field %s", name)
	return ""
}

func cliFreeTCPAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}

func cliAssertPortsReleased(t *testing.T, diagnostic, control, discovery string) {
	t.Helper()
	for _, address := range []string{diagnostic, control} {
		ln, err := net.Listen("tcp4", address)
		if err != nil {
			t.Fatalf("TCP listener remained bound at %s: %v", address, err)
		}
		ln.Close()
	}
	addr, err := net.ResolveUDPAddr("udp4", discovery)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Fatalf("UDP listener remained bound at %s: %v", discovery, err)
	}
	c.Close()
}

func cliExerciseTool(t *testing.T, diagnostic, control, discovery string, ident []byte, response []byte, kl15 byte) {
	t.Helper()
	discAddr, err := net.ResolveUDPAddr("udp4", discovery)
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	if err := udp.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := udp.WriteToUDP(hsfz.IdentRequest, discAddr); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	n, peer, err := udp.ReadFromUDP(buffer)
	if err != nil || !bytes.Equal(buffer[:n], ident) || err == nil && peer.String() != discovery {
		t.Fatalf("CLI discovery response: bytes=%d error=%v", n, err)
	}
	for _, tc := range []struct {
		address string
		request hsfz.Frame
		want    []hsfz.Frame
	}{
		{diagnostic, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}, []hsfz.Frame{
			{Control: hsfz.ControlAck, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}},
			{Control: hsfz.ControlDiag, Body: append([]byte{0x10, 0xf4}, response...)},
		}},
		{control, hsfz.Frame{Control: hsfz.ControlTerminal15}, []hsfz.Frame{{Control: hsfz.ControlTerminal15, Body: []byte{kl15}}}},
	} {
		conn, err := net.DialTimeout("tcp4", tc.address, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if err := hsfz.WriteFrame(conn, tc.request); err != nil {
			conn.Close()
			t.Fatal(err)
		}
		for _, want := range tc.want {
			got, err := hsfz.ReadFrame(conn, 1024)
			if err != nil || got.Control != want.Control || !bytes.Equal(got.Body, want.Body) {
				conn.Close()
				t.Fatalf("CLI TCP response at %s: got=%+v error=%v", tc.address, got, err)
			}
		}
		conn.Close()
	}
}

func TestRenetdCLISmoke(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	binary := filepath.Join(t.TempDir(), "renetd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/renetd")
	build.Dir = filepath.Join("..", "..")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build CLI: %v\n%s", err, output)
	}
	kl15 := byte(4)
	id := hsfz.Identification{VIN: "WBAXXXXXXX0000001", DiagAddr: 0x10}
	id.Raw = append(id.MarshalBody(), []byte("CLIFIXTURE42")...)
	response := append([]byte{0x62, 0xf1, 0x90}, []byte(id.VIN)...)
	zgw, err := fakezgw.Start(ctx, netip.MustParseAddr("127.0.0.2"), fakezgw.Profile{Ident: id, KL15: &kl15, Responders: map[string][]byte{"22f190": response}}, fakezgw.WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	defer zgw.Close()
	diag, ctrl, disc := zgw.Addr()
	ident, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: id.MarshalBody()}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	operatorDir, clientDir := t.TempDir(), t.TempDir()
	hubAddress := cliFreeTCPAddress(t)
	operatorArgs := []string{"operator", "--state-dir", operatorDir, "--listen", hubAddress, "--bind", "127.0.0.1", "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0", "--remote-diag-port", fmt.Sprint(diag.Port), "--remote-ctrl-port", fmt.Sprint(ctrl.Port)}
	operator := cliStart(t, ctx, binary, "", operatorArgs...)
	operator.waitLog(t, ctx, "operator ready")
	// Issuing after startup proves the running operator reloads invitation state
	// written by another process, rather than holding an obsolete in-memory copy.
	issue := exec.CommandContext(ctx, binary, "invite", "--state-dir", operatorDir, "--endpoint", hubAddress)
	var issueErrors bytes.Buffer
	issue.Stderr = &issueErrors
	code, err := issue.Output()
	if err != nil {
		t.Fatalf("issue invitation: %v\n%s", err, issueErrors.String())
	}
	secret := strings.TrimSpace(string(code))
	operator.logs.setSecret(secret)
	i, err := invite.Decode(secret)
	if err != nil {
		t.Fatalf("invalid invitation output: %v", err)
	}
	inviteFile := filepath.Join(t.TempDir(), "invitation.txt")
	if err := os.WriteFile(inviteFile, code, 0o600); err != nil {
		t.Fatal(err)
	}
	clientArgs := []string{"client", "--state-dir", clientDir, "--nic-addr", "127.0.0.1", "--discovery-target", disc.String(), "--discovery-timeout", "50ms"}
	client := cliStart(t, ctx, binary, secret, append(append([]string(nil), clientArgs...), "--invite-file", inviteFile)...)
	client.waitLog(t, ctx, "client connected")
	presenting := operator.waitLog(t, ctx, "operator presenting vehicle")
	diagnostic, control, discovery := cliField(t, presenting, "diagnostic"), cliField(t, presenting, "control"), cliField(t, presenting, "discovery")
	cliExerciseTool(t, diagnostic, control, discovery, ident, response, kl15)
	client.stop(t)
	operator.waitLog(t, ctx, "operator session ended")
	cliAssertPortsReleased(t, diagnostic, control, discovery)
	operator.stop(t)
	// Both processes restart: identities, trusted device state and the saved
	// operator endpoint must survive without using the already consumed token.
	operator = cliStart(t, ctx, binary, secret, operatorArgs...)
	operator.waitLog(t, ctx, "operator ready")
	client = cliStart(t, ctx, binary, secret, append(append([]string(nil), clientArgs...), "--peer", i.Peer.ID.String())...)
	client.waitLog(t, ctx, "client connected")
	presenting = operator.waitLog(t, ctx, "operator presenting vehicle")
	diagnostic, control, discovery = cliField(t, presenting, "diagnostic"), cliField(t, presenting, "control"), cliField(t, presenting, "discovery")
	cliExerciseTool(t, diagnostic, control, discovery, ident, response, kl15)
	client.stop(t)
	operator.waitLog(t, ctx, "operator session ended")
	cliAssertPortsReleased(t, diagnostic, control, discovery)
	operator.stop(t)
}
