package main

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/testutil/fakezgw"
)

type runtimeLog struct {
	mu      sync.Mutex
	buf     bytes.Buffer
	changed chan struct{}
}

func newRuntimeLog() *runtimeLog { return &runtimeLog{changed: make(chan struct{}, 1)} }
func (l *runtimeLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	n, err := l.buf.Write(p)
	l.mu.Unlock()
	select {
	case l.changed <- struct{}{}:
	default:
	}
	return n, err
}
func (l *runtimeLog) snapshot() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}
func (l *runtimeLog) wait(t *testing.T, markers ...string) string {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		text := l.snapshot()
		for _, line := range strings.Split(text, "\n") {
			match := true
			for _, marker := range markers {
				match = match && strings.Contains(line, marker)
			}
			if match {
				return line
			}
		}
		select {
		case <-l.changed:
		case <-timer.C:
			t.Fatalf("CLI log missing %q:\n%s", markers, text)
			return ""
		}
	}
}
func runtimeField(t *testing.T, line, key string) string {
	t.Helper()
	for _, field := range strings.Fields(line) {
		if value, ok := strings.CutPrefix(field, key+"="); ok {
			return value
		}
	}
	t.Fatalf("missing %s in %s", key, line)
	return ""
}
func runCLIForTest(t *testing.T, args []string) (*runtimeLog, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	logs := newRuntimeLog()
	done := make(chan error, 1)
	go func() { done <- run(ctx, args, io.Discard, logs) }()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("CLI exited unsuccessfully: %v\n%s", err, logs.snapshot())
				}
			case <-time.After(3 * time.Second):
				t.Error("CLI workers did not join")
			}
		})
	}
	t.Cleanup(stop)
	return logs, stop
}
func freeRuntimeHub(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

func TestCLIConnectionTestsAndStatusUseRealRuntime(t *testing.T) {
	for _, allow := range []bool{true, false} {
		t.Run(strconv.FormatBool(allow), func(t *testing.T) {
			_, gateway := gatewayState(t)
			zgw, err := fakezgw.Start(t.Context(), gateway.Address, fakezgw.Profile{Ident: gateway.Identification}, fakezgw.WithPorts(0, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			defer zgw.Close()
			diag, ctrl, disc := zgw.Addr()
			hub, operatorDir, clientDir := freeRuntimeHub(t), t.TempDir(), t.TempDir()
			capturePath := filepath.Join(t.TempDir(), "capture.jsonl")
			opArgs := []string{"operator", "--state-dir", operatorDir, "--listen", hub, "--bind", "127.0.0.1", "--diag-port", "0", "--ctrl-port", "0", "--disc-port", "0", "--remote-diag-port", strconv.Itoa(diag.Port), "--remote-ctrl-port", strconv.Itoa(ctrl.Port), "--exclusive=false", "--status-interval", "10ms", "--connection-test"}
			if allow {
				opArgs = append(opArgs, "--capture-hsfz", capturePath)
			}
			opLogs, stopOp := runCLIForTest(t, opArgs)
			opLogs.wait(t, "operator ready")
			var code bytes.Buffer
			if err := run(t.Context(), []string{"invite", "--state-dir", operatorDir, "--endpoint", hub}, &code, io.Discard); err != nil {
				t.Fatal(err)
			}
			secret := strings.TrimSpace(code.String())
			clientArgs := []string{"client", "--state-dir", clientDir, "--invite", secret, "--nic-addr", "127.0.0.1", "--discovery-target", disc.String(), "--discovery-timeout", "10ms", "--status-interval", "0", "--allow-link-test=" + strconv.FormatBool(allow)}
			clientLogs, stopClient := runCLIForTest(t, clientArgs)
			status, resultMessage := "ok", "connection test completed"
			if !allow {
				status, resultMessage = "unsupported", "connection test failed"
			}
			resultLine := opLogs.wait(t, resultMessage, "generation=1", "status="+status)
			if allow && runtimeField(t, resultLine, "samples") != "10" {
				t.Fatal("CLI did not perform actual probes")
			}
			if !allow && (strings.Contains(resultLine, "status=ok") || runtimeField(t, resultLine, "samples") != "0") {
				t.Fatal("unsupported test reported success")
			}
			if stats := zgw.Stats(); stats.Connections != 0 || stats.DiagReceived != 0 {
				t.Fatalf("CLI test reached vehicle: %+v", stats)
			}
			presenting := opLogs.wait(t, "operator presenting vehicle", "generation=1")
			c, err := net.DialTimeout("tcp4", runtimeField(t, presenting, "diagnostic"), time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(3 * time.Second))
			if err := hsfz.WriteFrame(c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22, 0xf1, 0x90}}); err != nil {
				t.Fatal(err)
			}
			if ack, err := hsfz.ReadFrame(c, 1024); err != nil || ack.Control != hsfz.ControlAck {
				t.Fatalf("ordinary CLI stream failed after connection test: %+v %v", ack, err)
			}
			statusLine := opLogs.wait(t, "runtime status", "phase=connected", "generation=1", "payload_bytes_in=11", "payload_bytes_out=11", "active_connections=1")
			for _, field := range []string{"rtt_valid", "rtt_samples", "in_bytes_per_second", "out_bytes_per_second", "exclusive_enabled"} {
				_ = runtimeField(t, statusLine, field)
			}
			c.Close()
			stopClient()
			opLogs.wait(t, "operator session ended")
			nextClientLogs, stopNext := runCLIForTest(t, clientArgs)
			opLogs.wait(t, resultMessage, "generation=2", "status="+status)
			if stats := zgw.Stats(); stats.Connections != 1 || stats.DiagReceived != 1 {
				t.Fatalf("reconnect test replayed data or reached vehicle: %+v", stats)
			}
			stopNext()
			stopOp()
			opLogs.wait(t, "runtime status", "phase=stopped")
			if strings.Contains(clientLogs.snapshot(), "runtime status") || strings.Contains(nextClientLogs.snapshot(), "runtime status") {
				t.Fatal("--status-interval=0 was ignored")
			}
			if strings.Contains(opLogs.snapshot(), secret) || strings.Contains(clientLogs.snapshot(), secret) {
				t.Fatal("CLI logs leaked invitation")
			}
			if allow {
				verifyCLICapture(t, capturePath, secret)
			} else if _, err := os.Stat(capturePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("CLI capture was not off by default")
			}
		})
	}
}

func verifyCLICapture(t *testing.T, path, invitation string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b, []byte(invitation)) {
		t.Fatal("session invitation entered capture")
	}
	var frames, payloadBytes int
	var last string
	decoder := json.NewDecoder(bytes.NewReader(b))
	for decoder.More() {
		var e struct{ Type, Data string }
		if err := decoder.Decode(&e); err != nil {
			t.Fatal(err)
		}
		last = e.Type
		if e.Type == "frame" {
			frames++
		}
		if e.Type == "data" {
			p, err := hex.DecodeString(e.Data)
			if err != nil {
				t.Fatal(err)
			}
			payloadBytes += len(p)
		}
	}
	if frames != 2 || payloadBytes != 22 || last != "capture_stop" {
		t.Fatalf("CLI capture did not drain exact HSFZ traffic: frames=%d bytes=%d last=%s", frames, payloadBytes, last)
	}
}

func TestCLICaptureRefusesExistingFileBeforeListening(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.jsonl")
	if err := os.WriteFile(path, []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	err := run(t.Context(), []string{"operator", "--state-dir", t.TempDir(), "--listen", freeRuntimeHub(t), "--bind", "127.0.0.1", "--exclusive=false", "--capture-hsfz", path}, io.Discard, &logs)
	if err == nil || strings.Contains(logs.String(), "operator ready") {
		t.Fatal("capture creation failure did not prevent startup")
	}
	b, _ := os.ReadFile(path)
	if string(b) != "evidence" {
		t.Fatal("existing evidence was modified")
	}
}

func TestCLIExclusiveUnsupportedFailsBeforeListening(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has a native resolver")
	}
	var logs bytes.Buffer
	err := run(t.Context(), []string{"operator", "--state-dir", t.TempDir(), "--listen", freeRuntimeHub(t), "--bind", "127.0.0.1", "--exclusive=true", "--status-interval", "0"}, io.Discard, &logs)
	if !errors.Is(err, exclusive.ErrUnsupported) || strings.Contains(logs.String(), "operator ready") {
		t.Fatalf("exclusive mode failed late: %v\n%s", err, logs.String())
	}
}
