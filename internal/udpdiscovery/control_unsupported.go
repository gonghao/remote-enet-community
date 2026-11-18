//go:build !linux && !darwin && !windows

package udpdiscovery

import (
	"fmt"
	"net"
	"runtime"
)

var layout = controlLayout{lengthBytes: 4, alignment: 4, payloadSize: 12}

const truncatedFlags = 0

func truncatedRead(error) bool { return false }

func enablePacketInfo(*net.UDPConn) error {
	return fmt.Errorf("packet-info discovery is unsupported on %s", runtime.GOOS)
}
