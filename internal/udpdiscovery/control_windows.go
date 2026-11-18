//go:build windows

package udpdiscovery

import (
	"errors"
	"net"
	"strconv"
	"syscall"
)

// WSACMSGHDR has a SIZE_T length; IN_PKTINFO stores address before interface index.
var layout = controlLayout{lengthBytes: strconv.IntSize / 8, alignment: strconv.IntSize / 8,
	packetType: 19, payloadSize: 8, indexOffset: 4, dstOffset: 0, srcOffset: 0}

const truncatedFlags = 0x0100 | 0x0200 // Winsock MSG_TRUNC | MSG_CTRUNC.

func truncatedRead(err error) bool { return errors.Is(err, syscall.Errno(10040)) }

func enablePacketInfo(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) { setErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, 19, 1) }); err != nil {
		return err
	}
	return setErr
}
