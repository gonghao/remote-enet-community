package carside

import (
	"context"
	"fmt"
	"net"
	"syscall"
)

func bindVehicleInterface(ctx context.Context, nic NIC, raw syscall.RawConn) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return fmt.Errorf("carside: enumerate socket interfaces: %w", err)
	}
	index, err := vehicleInterfaceIndex(nic, interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var optionErr error
	if err := raw.Control(func(fd uintptr) {
		optionErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_BOUND_IF, index)
	}); err != nil {
		return fmt.Errorf("carside: control vehicle socket: %w", err)
	}
	if optionErr != nil {
		return fmt.Errorf("carside: bind vehicle socket interface: %w", optionErr)
	}
	return ctx.Err()
}
