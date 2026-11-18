//go:build windows

package exclusive

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// A separate client process distinguishes its reverse row from the accepted
// server row. Both sockets stay live until all identity assertions finish.
func TestNativeOwnerResolverReverseTuple(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := ln.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestNativeOwnerClientProcess$")
	cmd.Env = append(os.Environ(), "RENET_OWNER_TEST_CLIENT="+ln.Addr().String())
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		input.Close()
		t.Fatal(err)
	}
	waited := false
	defer func() {
		input.Close()
		if !waited {
			if err := cmd.Wait(); err != nil {
				t.Errorf("client process: %v", err)
			}
		}
	}()
	conn, err := ln.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	local := conn.LocalAddr().(*net.TCPAddr).AddrPort()
	remote := conn.RemoteAddr().(*net.TCPAddr).AddrPort()
	r := NewResolver()
	owner, err := r.Resolve(ctx, local, remote)
	if err != nil {
		t.Fatal(err)
	}
	if owner.PID != uint32(cmd.Process.Pid) || owner.PID == uint32(os.Getpid()) || owner.Created == 0 {
		t.Fatalf("owner=%+v, client PID=%d, server PID=%d", owner, cmd.Process.Pid, os.Getpid())
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(cmd.Process.Pid))
	if err != nil {
		t.Fatal(err)
	}
	var created, exited, kernel, user syscall.Filetime
	err = syscall.GetProcessTimes(handle, &created, &exited, &kernel, &user)
	syscall.CloseHandle(handle)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)
	if owner.Created != wantCreated {
		t.Fatalf("creation time=%d, OS process creation=%d", owner.Created, wantCreated)
	}
	if again, err := r.Resolve(ctx, local, remote); err != nil || again != owner {
		t.Fatalf("unstable identity: %+v, %v", again, err)
	}
	t.Logf("client PID=%d creation FILETIME=%d; server PID=%d; accepted %v <- %v", owner.PID, owner.Created, os.Getpid(), local, remote)

	missingCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = r.Resolve(missingCtx, local, local)
	stop()
	if !errors.Is(err, ErrUnknownOwner) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("missing reverse row: %v", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := r.Resolve(canceled, local, remote); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled resolve: %v", err)
	}
	input.Close()
	err = cmd.Wait()
	waited = true
	if err != nil {
		t.Fatal(err)
	}
	exitedCtx, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	_, err = r.Resolve(exitedCtx, local, remote)
	stop()
	if err == nil {
		t.Fatal("exited client still resolved as a live owner")
	}
}

func TestNativeOwnerClientProcess(t *testing.T) {
	address := os.Getenv("RENET_OWNER_TEST_CLIENT")
	if address == "" {
		t.Skip("subprocess helper")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", address)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.Copy(io.Discard, os.Stdin); err != nil {
		t.Fatal(err)
	}
}
