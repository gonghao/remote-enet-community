//go:build windows

package exclusive

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"syscall"
	"unsafe"
)

// iphlpapi.dll is also used (and registered as a system DLL) by syscall.
var getExtendedTCPTable = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

type systemResolver struct{}

func (systemResolver) Resolve(ctx context.Context, local, remote netip.AddrPort) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	local = netip.AddrPortFrom(local.Addr().Unmap(), local.Port())
	remote = netip.AddrPortFrom(remote.Addr().Unmap(), remote.Port())
	if !local.IsValid() || !remote.IsValid() || local.Port() == 0 || remote.Port() == 0 {
		return Identity{}, ErrUnknownOwner
	}
	if !local.Addr().Is4() || !remote.Addr().Is4() {
		return Identity{}, ErrUnsupported
	}
	if err := getExtendedTCPTable.Find(); err != nil {
		return Identity{}, fmt.Errorf("exclusive: find GetExtendedTcpTable: %w", err)
	}
	return resolveOwner(ctx, func(ctx context.Context) (Identity, error) {
		return resolveTCP4(ctx, local, remote)
	})
}

func resolveTCP4(ctx context.Context, local, remote netip.AddrPort) (Identity, error) {
	pid, err := queryTCP4(ctx, local, remote)
	if err != nil {
		return Identity{}, err
	}
	const processQueryLimitedInformation = 0x1000
	handle, err := syscall.OpenProcess(processQueryLimitedInformation|syscall.SYNCHRONIZE, false, pid)
	if err != nil {
		if errors.Is(err, syscall.Errno(87)) { // ERROR_INVALID_PARAMETER: process exited.
			return Identity{}, fmt.Errorf("exclusive: open process %d: %w: %w", pid, ErrUnknownOwner, err)
		}
		return Identity{}, fmt.Errorf("exclusive: open process %d: %w", pid, err)
	}
	defer syscall.CloseHandle(handle)
	var created, exited, kernel, user syscall.Filetime
	if err := syscall.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		return Identity{}, fmt.Errorf("exclusive: process %d creation time: %w", pid, err)
	}
	owner := Identity{PID: pid, Created: uint64(created.HighDateTime)<<32 | uint64(created.LowDateTime)}
	if owner.Created == 0 {
		return Identity{}, ErrUnknownOwner
	}
	// Keep the process handle while rechecking the socket and liveness. A
	// stale PID lookup must not attach an old connection to a new process.
	if err := processRunning(handle); err != nil {
		return Identity{}, err
	}
	currentPID, err := queryTCP4(ctx, local, remote)
	if err != nil {
		return Identity{}, err
	}
	if currentPID != pid {
		return Identity{}, ErrUnknownOwner
	}
	if err := processRunning(handle); err != nil {
		return Identity{}, err
	}
	return owner, nil
}

func processRunning(handle syscall.Handle) error {
	state, err := syscall.WaitForSingleObject(handle, 0)
	if err != nil {
		return fmt.Errorf("exclusive: query process liveness: %w", err)
	}
	if state != syscall.WAIT_TIMEOUT {
		return ErrUnknownOwner
	}
	return nil
}

func queryTCP4(ctx context.Context, local, remote netip.AddrPort) (uint32, error) {
	const maxTableSize = 16 << 20
	size := uint32(16 << 10)
	for range 8 {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if size < 4 || size > maxTableSize {
			return 0, fmt.Errorf("exclusive: TCP table size %d outside 4..%d", size, maxTableSize)
		}
		table := make([]byte, int(size))
		const tcpTableOwnerPIDConnections = 4
		status, _, _ := getExtendedTCPTable.Call(uintptr(unsafe.Pointer(&table[0])), uintptr(unsafe.Pointer(&size)),
			0, syscall.AF_INET, tcpTableOwnerPIDConnections, 0)
		if syscall.Errno(status) == syscall.ERROR_INSUFFICIENT_BUFFER {
			continue
		}
		if status != 0 {
			return 0, fmt.Errorf("exclusive: GetExtendedTcpTable: %w", syscall.Errno(status))
		}
		if size > uint32(len(table)) {
			return 0, fmt.Errorf("exclusive: invalid TCP table size %d", size)
		}
		return tcp4Owner(table[:size], local, remote)
	}
	return 0, fmt.Errorf("exclusive: TCP table changed during all query attempts")
}
