package exclusive

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

// MIB_TCPTABLE_OWNER_PID has a DWORD count followed by 24-byte rows. DWORDs
// are little-endian, but in_addr and the low two port bytes are network order.
func tcp4Owner(table []byte, acceptedLocal, acceptedRemote netip.AddrPort) (uint32, error) {
	const rowSize = 24
	if len(table) < 4 {
		return 0, fmt.Errorf("exclusive: truncated TCP table")
	}
	count := binary.LittleEndian.Uint32(table[:4])
	if uint64(count) > uint64((len(table)-4)/rowSize) {
		return 0, fmt.Errorf("exclusive: truncated TCP rows")
	}
	var owner uint32
	for i := uint32(0); i < count; i++ {
		row := table[4+int(i)*rowSize:][:rowSize]
		state := binary.LittleEndian.Uint32(row[:4])
		// ESTABLISHED through LAST_ACK retain an endpoint, including half-close.
		// TIME_WAIT has no live socket owner and must never grant admission.
		if state < 5 || state > 10 {
			continue
		}
		local := netip.AddrPortFrom(netip.AddrFrom4([4]byte(row[4:8])), binary.BigEndian.Uint16(row[8:10]))
		remote := netip.AddrPortFrom(netip.AddrFrom4([4]byte(row[12:16])), binary.BigEndian.Uint16(row[16:18]))
		// The accepted/server row belongs to the Presenter itself. Only its
		// exact reverse identifies the local diagnostic client's socket.
		if local != acceptedRemote || remote != acceptedLocal {
			continue
		}
		pid := binary.LittleEndian.Uint32(row[20:24])
		if pid == 0 || (owner != 0 && owner != pid) {
			return 0, ErrUnknownOwner
		}
		owner = pid
	}
	if owner == 0 {
		return 0, ErrUnknownOwner
	}
	return owner, nil
}
