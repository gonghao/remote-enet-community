//go:build darwin

package udpdiscovery

import (
	"net"
	"syscall"
)

// Darwin retains a 32-bit cmsg length and alignment on both supported architectures.
var layout = controlLayout{lengthBytes: 4, alignment: 4,
	packetType: syscall.IP_PKTINFO, payloadSize: 12, indexOffset: 0, dstOffset: 8, srcOffset: 4}

const truncatedFlags = syscall.MSG_TRUNC | syscall.MSG_CTRUNC

func truncatedRead(error) bool { return false }

func enablePacketInfo(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) {
		setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_RECVPKTINFO, 1)
	}); err != nil {
		return err
	}
	return setErr
}
