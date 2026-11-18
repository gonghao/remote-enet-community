//go:build windows

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/operator"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

const (
	nativeExclusiveStep       = 5 * time.Second
	nativeExclusiveModeEnv    = "RENET_NATIVE_EXCLUSIVE_MODE"
	nativeExclusiveTargetEnv  = "RENET_NATIVE_EXCLUSIVE_TARGET"
	nativeExclusiveControlEnv = "RENET_NATIVE_EXCLUSIVE_CONTROL"
)

func TestNativeExclusiveMultiProcess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	opener := newNativeExclusiveEchoOpener(t, ctx)
	rejections := make(nativeExclusiveRejections, 8)
	ident := hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, VIN: "WBAXXXXXXX0000001"}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: ident.MarshalBody()}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	p, err := operator.Start(ctx, operator.Options{
		Bind:      netip.MustParseAddr("127.0.0.1"),
		Exclusive: true,
		// A nil Resolver must select the real Windows TCP table implementation.
		State:  proto.VehicleState{GatewayAddr: "127.0.0.1", IdentRaw: raw},
		Opener: opener,
		Logger: slog.New(slog.NewJSONHandler(rejections, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Error(err)
		}
	})
	diag, ctrl, disc := p.Addr()
	parent, err := nativeExclusiveIdentity(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	d := nativeExclusiveDial(t, ctx, diag.String())
	nativeExclusiveEcho(t, d, "parent diagnostic")
	opener.expect(t, ctx, d, "127.0.0.1:6801", 1)
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true, Owner: parent, Active: 1})
	c := nativeExclusiveDial(t, ctx, ctrl.String())
	nativeExclusiveEcho(t, c, "parent control")
	opener.expect(t, ctx, c, "127.0.0.1:6811", 2)
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true, Owner: parent, Active: 2})
	t.Logf("parent owner: PID=%d creation FILETIME=%d, diagnostic=%s control=%s", parent.PID, parent.Created, diag, ctrl)

	reject := func(target string, active int) {
		child := startNativeExclusiveChild(t, ctx, target, "reject")
		connected := child.receive(t, "connected")
		rejections.expectBusy(t, ctx, connected.Local)
		child.send(t, "check")
		child.receive(t, "rejected")
		opener.expectCount(t, 2)
		nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true, Owner: parent, Active: active})
		t.Logf("rejected PID=%d creation FILETIME=%d at %s; parent retains %d connections", child.owner.PID, child.owner.Created, connected.Local, active)
		child.finish(t)
	}
	reject(diag.String(), 2)
	// Closing only diagnostic must leave the control connection's owner intact.
	nativeExclusiveDrain(t, d)
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true, Owner: parent, Active: 1})
	reject(ctrl.String(), 1)
	nativeExclusiveEcho(t, c, "control still owns gateway")
	nativeExclusiveDrain(t, c)
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true})

	child := startNativeExclusiveChild(t, ctx, diag.String(), "echo")
	connected := child.receive(t, "connected")
	child.send(t, "echo")
	child.receive(t, "echoed")
	opener.expectOrigin(t, ctx, connected.Local, "127.0.0.1:6801", 3)
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true, Owner: child.owner, Active: 1})
	t.Logf("replacement owner: PID=%d creation FILETIME=%d at %s", child.owner.PID, child.owner.Created, connected.Local)
	child.send(t, "close")
	child.receive(t, "closed")
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true})
	child.finish(t)
	opener.expectCount(t, 3)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	nativeExclusiveState(t, ctx, p, exclusive.Snapshot{Enabled: true})
	assertPortsReleased(t, diag, ctrl, disc)
}

type nativeExclusiveEchoOpener struct {
	transport.Session
	opens    atomic.Uint64
	requests chan proto.OpenTunnel
}

func newNativeExclusiveEchoOpener(t *testing.T, ctx context.Context) *nativeExclusiveEchoOpener {
	t.Helper()
	local, remote := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	ctx, cancel := context.WithCancel(ctx)
	o := &nativeExclusiveEchoOpener{Session: local, requests: make(chan proto.OpenTunnel, 8)}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			st, err := remote.AcceptStream(ctx)
			if err != nil {
				return
			}
			workers.Go(func() {
				defer st.Close()
				if _, err := io.Copy(st, st); err == nil {
					_ = st.CloseWrite()
				}
			})
		}
	})
	t.Cleanup(func() {
		cancel()
		_ = local.Close(nil)
		_ = remote.Close(nil)
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(nativeExclusiveStep):
			t.Error("echo workers did not stop")
		}
	})
	return o
}

func (o *nativeExclusiveEchoOpener) OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
	o.opens.Add(1)
	select {
	case o.requests <- h:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// All upstream traffic stays in memory, regardless of the header target.
	return o.Session.OpenStream(ctx)
}

func (o *nativeExclusiveEchoOpener) expect(t *testing.T, ctx context.Context, c *net.TCPConn, target string, count uint64) {
	t.Helper()
	o.expectOrigin(t, ctx, c.LocalAddr().String(), target, count)
}

func (o *nativeExclusiveEchoOpener) expectOrigin(t *testing.T, ctx context.Context, origin, target string, count uint64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, nativeExclusiveStep)
	defer cancel()
	select {
	case h := <-o.requests:
		if h.Kind != "tcp" || h.Target != target || h.Origin != origin {
			t.Fatalf("unexpected upstream tunnel: %+v", h)
		}
	case <-ctx.Done():
		t.Fatalf("upstream open: %v", ctx.Err())
	}
	o.expectCount(t, count)
}

func (o *nativeExclusiveEchoOpener) expectCount(t *testing.T, want uint64) {
	t.Helper()
	if got := o.opens.Load(); got != want {
		t.Fatalf("upstream opens=%d, want %d", got, want)
	}
}

func nativeExclusiveState(t *testing.T, ctx context.Context, p *operator.Presenter, want exclusive.Snapshot) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, nativeExclusiveStep)
	defer cancel()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if got := p.ExclusiveState(); got == want {
			return
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			t.Fatalf("exclusive state=%+v, want %+v: %v", p.ExclusiveState(), want, ctx.Err())
		}
	}
}

func nativeExclusiveDial(t *testing.T, ctx context.Context, address string) *net.TCPConn {
	t.Helper()
	c, err := nativeExclusiveConnect(ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	t.Cleanup(func() { stop(); _ = c.Close() })
	return c
}

func nativeExclusiveConnect(ctx context.Context, address string) (*net.TCPConn, error) {
	endpoint, err := netip.ParseAddrPort(address)
	if err != nil || endpoint.Addr() != netip.MustParseAddr("127.0.0.1") || endpoint.Port() == 0 {
		return nil, fmt.Errorf("invalid native test loopback endpoint %q", address)
	}
	c, err := (&net.Dialer{Timeout: nativeExclusiveStep}).DialContext(ctx, "tcp4", endpoint.String())
	if err != nil {
		return nil, err
	}
	return c.(*net.TCPConn), nil
}

func nativeExclusiveDeadline(t *testing.T, c *net.TCPConn) {
	t.Helper()
	if err := c.SetDeadline(time.Now().Add(nativeExclusiveStep)); err != nil {
		t.Fatal(err)
	}
}

func nativeExclusiveEcho(t *testing.T, c *net.TCPConn, text string) {
	t.Helper()
	nativeExclusiveDeadline(t, c)
	want := []byte(text)
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("echo=%q, want %q", got, want)
	}
}

func nativeExclusiveDrain(t *testing.T, c *net.TCPConn) {
	t.Helper()
	nativeExclusiveDeadline(t, c)
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	var extra [1]byte
	if n, err := c.Read(extra[:]); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("half-close drain: bytes=%d error=%v", n, err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
}

type nativeExclusiveRejection struct {
	Message string `json:"msg"`
	Peer    string `json:"peer"`
	Error   string `json:"error"`
}

// Observing ErrBusy distinguishes rejection from a failed native table query.
type nativeExclusiveRejections chan nativeExclusiveRejection

func (r nativeExclusiveRejections) Write(p []byte) (int, error) {
	var event nativeExclusiveRejection
	if err := json.Unmarshal(p, &event); err != nil {
		return 0, err
	}
	if event.Message == "gateway connection rejected" {
		select {
		case r <- event:
		default:
			return 0, fmt.Errorf("native rejection log overflow")
		}
	}
	return len(p), nil
}

func (r nativeExclusiveRejections) expectBusy(t *testing.T, ctx context.Context, peer string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, nativeExclusiveStep)
	defer cancel()
	select {
	case event := <-r:
		if event.Peer != peer || event.Error != exclusive.ErrBusy.Error() {
			t.Fatalf("expected native busy rejection for %s: %+v", peer, event)
		}
	case <-ctx.Done():
		t.Fatalf("native busy rejection for %s: %v", peer, ctx.Err())
	}
}

type nativeExclusiveMessage struct {
	Phase  string
	Owner  exclusive.Identity
	Local  string
	Remote string
}

type nativeExclusiveChild struct {
	conn    *net.TCPConn
	decoder *json.Decoder
	encoder *json.Encoder
	owner   exclusive.Identity
	target  string
	cancel  context.CancelFunc
	done    chan struct{}
	err     error
	output  nativeExclusiveOutput
}

// The extra listener is a control channel only; Presenter still receives real
// client sockets owned by the independently launched test process.
func startNativeExclusiveChild(t *testing.T, ctx context.Context, target, mode string) *nativeExclusiveChild {
	t.Helper()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := ln.SetDeadline(time.Now().Add(nativeExclusiveStep)); err != nil {
		t.Fatal(err)
	}
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	child := &nativeExclusiveChild{cancel: cancel, done: make(chan struct{}), target: target}
	cmd := exec.CommandContext(childCtx, executable, "-test.run=^TestNativeExclusiveProcessHelper$", "-test.timeout=20s")
	cmd.Env = append(os.Environ(), nativeExclusiveModeEnv+"="+mode, nativeExclusiveTargetEnv+"="+target, nativeExclusiveControlEnv+"="+ln.Addr().String())
	cmd.Stdout, cmd.Stderr = &child.output, &child.output
	cmd.WaitDelay = time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	go func() { child.err = cmd.Wait(); close(child.done) }()
	t.Cleanup(func() {
		child.cancel()
		if child.conn != nil {
			_ = child.conn.Close()
		}
		select {
		case <-child.done:
		case <-time.After(nativeExclusiveStep):
			t.Error("native helper process was not reaped")
		}
	})
	child.conn, err = ln.AcceptTCP()
	if err != nil {
		t.Fatalf("native helper control connection: %v; output: %s", err, child.output.String())
	}
	stopControl := context.AfterFunc(childCtx, func() { _ = child.conn.Close() })
	t.Cleanup(func() { stopControl() })
	child.decoder, child.encoder = json.NewDecoder(child.conn), json.NewEncoder(child.conn)
	ready := child.receive(t, "ready")
	child.owner, err = nativeExclusiveIdentity(uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	if ready.Owner != child.owner || child.owner.PID == uint32(os.Getpid()) {
		t.Fatalf("native helper identity=%+v, process identity=%+v", ready.Owner, child.owner)
	}
	child.send(t, "connect")
	return child
}

func (c *nativeExclusiveChild) receive(t *testing.T, phase string) nativeExclusiveMessage {
	t.Helper()
	nativeExclusiveDeadline(t, c.conn)
	var message nativeExclusiveMessage
	if err := c.decoder.Decode(&message); err != nil {
		t.Fatalf("native helper phase %s: %v; output: %s", phase, err, c.output.String())
	}
	if message.Phase != phase {
		t.Fatalf("native helper phase=%q, want %q", message.Phase, phase)
	}
	if phase == "connected" {
		if message.Owner != c.owner {
			t.Fatalf("native helper changed identity: %+v", message)
		}
		for _, address := range []string{message.Local, message.Remote} {
			endpoint, err := netip.ParseAddrPort(address)
			if err != nil || endpoint.Addr() != netip.MustParseAddr("127.0.0.1") || endpoint.Port() == 0 {
				t.Fatalf("native helper reported non-loopback endpoint %q", address)
			}
		}
		if message.Remote != c.target {
			t.Fatalf("native helper connected to %q, want %q", message.Remote, c.target)
		}
	}
	return message
}

func (c *nativeExclusiveChild) send(t *testing.T, command string) {
	t.Helper()
	nativeExclusiveDeadline(t, c.conn)
	if err := c.encoder.Encode(command); err != nil {
		t.Fatalf("native helper command %s: %v; output: %s", command, err, c.output.String())
	}
}

func (c *nativeExclusiveChild) finish(t *testing.T) {
	t.Helper()
	c.send(t, "exit")
	select {
	case <-c.done:
		if c.err != nil {
			t.Fatalf("native helper exit: %v; output: %s", c.err, c.output.String())
		}
	case <-time.After(nativeExclusiveStep):
		t.Fatalf("native helper did not exit; output: %s", c.output.String())
	}
	_ = c.conn.Close()
}

type nativeExclusiveOutput struct {
	mu   sync.Mutex
	data []byte
}

func (o *nativeExclusiveOutput) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	const limit = 64 << 10
	o.data = append(o.data, p[:min(len(p), limit-len(o.data))]...)
	return len(p), nil
}

func (o *nativeExclusiveOutput) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return string(o.data)
}

func nativeExclusiveIdentity(pid uint32) (exclusive.Identity, error) {
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return exclusive.Identity{}, fmt.Errorf("query native test process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(handle)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return exclusive.Identity{}, fmt.Errorf("native test creation time: %w", err)
	}
	owner := exclusive.Identity{PID: pid, Created: uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)}
	if owner.PID == 0 || owner.Created == 0 {
		return exclusive.Identity{}, fmt.Errorf("invalid native process identity %+v", owner)
	}
	return owner, nil
}

func TestNativeExclusiveProcessHelper(t *testing.T) {
	mode := os.Getenv(nativeExclusiveModeEnv)
	if mode == "" {
		t.Skip("subprocess helper")
	}
	if mode != "reject" && mode != "echo" {
		t.Fatalf("invalid native helper mode %q", mode)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	control := nativeExclusiveDial(t, ctx, os.Getenv(nativeExclusiveControlEnv))
	decoder, encoder := json.NewDecoder(control), json.NewEncoder(control)
	command := func(want string) {
		nativeExclusiveDeadline(t, control)
		var got string
		if err := decoder.Decode(&got); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("native helper command=%q, want %q", got, want)
		}
	}
	owner, err := nativeExclusiveIdentity(uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	emit := func(phase string, c *net.TCPConn) {
		nativeExclusiveDeadline(t, control)
		message := nativeExclusiveMessage{Phase: phase, Owner: owner}
		if c != nil {
			message.Local, message.Remote = c.LocalAddr().String(), c.RemoteAddr().String()
		}
		if err := encoder.Encode(message); err != nil {
			t.Fatal(err)
		}
	}
	emit("ready", nil)
	command("connect")
	conn := nativeExclusiveDial(t, ctx, os.Getenv(nativeExclusiveTargetEnv))
	emit("connected", conn)
	if mode == "reject" {
		command("check")
		nativeExclusiveDeadline(t, conn)
		var b [1]byte
		n, err := conn.Read(b[:])
		var ne net.Error
		if n != 0 || err == nil || (errors.As(err, &ne) && ne.Timeout()) {
			t.Fatalf("native competitor was not closed: bytes=%d error=%v", n, err)
		}
		if err := conn.Close(); err != nil {
			t.Fatal(err)
		}
		emit("rejected", nil)
	} else {
		command("echo")
		nativeExclusiveEcho(t, conn, "replacement process echo")
		emit("echoed", nil)
		command("close")
		nativeExclusiveDrain(t, conn)
		emit("closed", nil)
	}
	command("exit")
}
