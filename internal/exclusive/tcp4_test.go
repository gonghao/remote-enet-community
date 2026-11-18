package exclusive

import (
	"encoding/binary"
	"errors"
	"net/netip"
	"testing"
)

func tcpRow(state, pid uint32, local, remote netip.AddrPort) []byte {
	row := make([]byte, 24)
	binary.LittleEndian.PutUint32(row[:4], state)
	ip := local.Addr().As4()
	copy(row[4:8], ip[:])
	binary.BigEndian.PutUint16(row[8:10], local.Port())
	ip = remote.Addr().As4()
	copy(row[12:16], ip[:])
	binary.BigEndian.PutUint16(row[16:18], remote.Port())
	binary.LittleEndian.PutUint32(row[20:24], pid)
	return row
}

func tcpTable(rows ...[]byte) []byte {
	table := make([]byte, 4)
	binary.LittleEndian.PutUint32(table, uint32(len(rows)))
	for _, row := range rows {
		table = append(table, row...)
	}
	return table
}

func TestTCP4ReverseTupleAndByteOrder(t *testing.T) {
	local := netip.MustParseAddrPort("10.21.32.43:6801")
	remote := netip.MustParseAddrPort("10.21.32.43:51345")
	otherIP := netip.MustParseAddrPort("127.0.0.1:6801")
	otherPort := netip.MustParseAddrPort("10.21.32.43:6811")
	table := tcpTable(
		tcpRow(5, 11, local, remote), // Server PID must never be returned.
		tcpRow(5, 12, remote, otherIP),
		tcpRow(5, 13, remote, otherPort),
		tcpRow(5, 14, otherIP, local),
		tcpRow(5, 123456, remote, local),
	)
	pid, err := tcp4Owner(table, local, remote)
	if err != nil || pid != 123456 {
		t.Fatalf("reverse tuple PID=%d err=%v", pid, err)
	}
}

func TestTCP4StatesAndUnknownOwner(t *testing.T) {
	for state := uint32(1); state <= 12; state++ {
		pid, err := tcp4Owner(tcpTable(tcpRow(state, 42, testRemote, testLocal)), testLocal, testRemote)
		if state >= 5 && state <= 10 {
			if err != nil || pid != 42 {
				t.Fatalf("state %d: PID=%d err=%v", state, pid, err)
			}
		} else if !errors.Is(err, ErrUnknownOwner) {
			t.Fatalf("state %d granted ownership: PID=%d err=%v", state, pid, err)
		}
	}
	for _, table := range [][]byte{
		tcpTable(),
		tcpTable(tcpRow(5, 0, testRemote, testLocal)),
		tcpTable(tcpRow(5, 1, testLocal, testRemote)),
		tcpTable(tcpRow(5, 1, testRemote, testLocal), tcpRow(5, 2, testRemote, testLocal)),
	} {
		if pid, err := tcp4Owner(table, testLocal, testRemote); pid != 0 || !errors.Is(err, ErrUnknownOwner) {
			t.Fatalf("unknown/ambiguous owner: PID=%d err=%v", pid, err)
		}
	}
}

func TestTCP4MalformedTables(t *testing.T) {
	for _, table := range [][]byte{
		nil, {1}, {1, 0, 0, 0}, {255, 255, 255, 255},
		append([]byte{2, 0, 0, 0}, make([]byte, 24)...),
	} {
		if _, err := tcp4Owner(table, testLocal, testRemote); err == nil {
			t.Fatalf("malformed table accepted: %x", table)
		}
	}
}
