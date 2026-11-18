package netpriv

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"remote-enet/internal/packetio"
	"remote-enet/internal/tunadapter"
)

func peerUID(c *net.UnixConn) (uint32, error) {
	raw, e := c.SyscallConn()
	if e != nil {
		return 0, e
	}
	var uid uint32
	var query error
	e = raw.Control(func(fd uintptr) {
		var u *unix.Xucred
		u, query = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if query == nil {
			uid = u.Uid
		}
	})
	return uid, errors.Join(e, query)
}
func openAuthorized(ctx context.Context, target netip.Addr, o Options) (*Device, error) {
	helper, e := filepath.EvalSymlinks(o.HelperPath)
	if e != nil || !validHelper(helper) {
		return nil, fmt.Errorf("netpriv: invalid bundled helper: %w", os.ErrPermission)
	}
	dir, e := os.MkdirTemp("", "renet-net-")
	if e != nil {
		return nil, fmt.Errorf("netpriv: create private IPC directory")
	}
	defer os.RemoveAll(dir)
	canonical, e := filepath.EvalSymlinks(dir)
	if e != nil || strings.IndexFunc(canonical, func(r rune) bool { return r < 32 || r == 127 }) >= 0 {
		return nil, fmt.Errorf("netpriv: resolve IPC directory")
	}
	socket := filepath.Join(canonical, "ipc")
	listener, e := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if e != nil {
		return nil, fmt.Errorf("netpriv: listen for helper")
	}
	defer listener.Close()
	if e = os.Chmod(socket, 0600); e != nil {
		return nil, fmt.Errorf("netpriv: secure IPC socket")
	}
	// The authorization process lives with the resource context, not a setup timeout.
	launchCtx, launchCancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(launchCtx, "/usr/bin/osascript", "-e", authorizationScript(helper, socket, os.Getuid()))
	cmd.WaitDelay = time.Second
	if e = cmd.Start(); e != nil {
		launchCancel()
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("netpriv: authorization launch failed: %w", os.ErrPermission)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait(); _ = listener.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	c, e := listener.AcceptUnix()
	if e != nil {
		launchCancel()
		<-exited
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("netpriv: authorization denied or helper launch failed: %w", os.ErrPermission)
	}
	uid, e := peerUID(c)
	if e != nil || uid != 0 {
		_ = c.Close()
		launchCancel()
		<-exited
		return nil, fmt.Errorf("netpriv: untrusted helper: %w", os.ErrPermission)
	}
	d, e := handshake(ctx, c, target)
	if e != nil {
		_ = c.Close()
		launchCancel()
		<-exited
		return nil, e
	}
	// Reap osascript when the helper exits. Closing IPC is the helper's cleanup signal;
	// killing osascript is never relied on to terminate a privileged descendant.
	base := d.Device
	d.Device = &launchedDevice{Device: base, cancel: launchCancel, exited: exited}
	return d, nil
}

type launchedDevice struct {
	packetio.Device
	cancel context.CancelFunc
	exited <-chan error
	once   sync.Once
	err    error
}

func (d *launchedDevice) Close() error {
	d.once.Do(func() {
		d.err = d.Device.Close()
		select {
		case e := <-d.exited:
			if e != nil {
				d.err = errors.Join(d.err, fmt.Errorf("netpriv: authorization process failed"))
			}
		case <-time.After(2 * time.Second):
			d.cancel()
			d.err = errors.Join(d.err, fmt.Errorf("netpriv: authorization process did not exit"))
		}
		d.cancel()
	})
	return d.err
}

// RunHelper accepts only private IPC coordinates; the target arrives over authenticated IPC.
func RunHelper(ctx context.Context, args []string) error {
	if len(args) != 4 || args[0] != "--ipc" || args[2] != "--uid" || !isElevated() {
		return fmt.Errorf("netpriv: invalid helper invocation")
	}
	uid, e := strconv.ParseUint(args[3], 10, 32)
	if e != nil || uid == 0 {
		return fmt.Errorf("netpriv: invalid helper identity")
	}
	socket := args[1]
	dir := filepath.Dir(socket)
	if !filepath.IsAbs(socket) || filepath.Base(socket) != "ipc" || !strings.HasPrefix(filepath.Base(dir), "renet-net-") {
		return fmt.Errorf("netpriv: invalid IPC location")
	}
	canonical, e := filepath.EvalSymlinks(dir)
	if e != nil || canonical != dir {
		return fmt.Errorf("netpriv: invalid IPC directory")
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return fmt.Errorf("netpriv: insecure IPC directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uid {
		return fmt.Errorf("netpriv: invalid IPC owner")
	}
	info, e = os.Lstat(socket)
	if e != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0600 {
		return fmt.Errorf("netpriv: insecure IPC socket")
	}
	stat, ok = info.Sys().(*syscall.Stat_t)
	if !ok || uint64(stat.Uid) != uid {
		return fmt.Errorf("netpriv: invalid socket owner")
	}
	dialer := net.Dialer{Timeout: handshakeTimeout}
	c, e := dialer.DialContext(ctx, "unix", socket)
	if e != nil {
		return fmt.Errorf("netpriv: IPC unavailable")
	}
	defer c.Close()
	peer, e := peerUID(c.(*net.UnixConn))
	if e != nil || uint64(peer) != uid {
		return fmt.Errorf("netpriv: untrusted IPC peer")
	}
	return serve(ctx, c, func(ctx context.Context, targets []netip.Addr) (ownedDevice, error) {
		a, e := tunadapter.OpenAuto(ctx, targets)
		if e != nil {
			return nil, e
		}
		return a, nil
	})
}
