//go:build linux

package udpdiscovery

import (
	"net"
	"strconv"
	"syscall"
)

var layout = controlLayout{lengthBytes: strconv.IntSize / 8, alignment: strconv.IntSize / 8,
	packetType: syscall.IP_PKTINFO, payloadSize: 12, indexOffset: 0, dstOffset: 8, srcOffset: 4}

const truncatedFlags = syscall.MSG_TRUNC | syscall.MSG_CTRUNC

func truncatedRead(error) bool { return false }

func enablePacketInfo(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var setErr error
	if err := raw.Control(func(fd uintptr) { setErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IP, syscall.IP_PKTINFO, 1) }); err != nil {
		return err
	}
	return setErr
}
