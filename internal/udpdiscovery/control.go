package udpdiscovery

import (
	"encoding/binary"
	"fmt"
	"net/netip"
)

type packetInfo struct {
	dst   netip.Addr
	index int
}

// All layouts use native-endian cmsg integers and network-order IPv4 bytes.
type controlLayout struct {
	lengthBytes int
	alignment   int
	packetType  uint32
	payloadSize int
	indexOffset int
	dstOffset   int
	srcOffset   int
}

func (l controlLayout) headerSize() int { return l.lengthBytes + 8 }

func (l controlLayout) align(n int) int { return (n + l.alignment - 1) & ^(l.alignment - 1) }

func parsePacketInfo(raw []byte) (packetInfo, error) {
	return parseControl(layout, raw)
}

func parseControl(layout controlLayout, raw []byte) (packetInfo, error) {
	var result packetInfo
	found := false
	header := layout.headerSize()
	for len(raw) != 0 {
		if len(raw) < header {
			return packetInfo{}, fmt.Errorf("udpdiscovery: truncated control header")
		}
		var length uint64
		if layout.lengthBytes == 8 {
			length = binary.NativeEndian.Uint64(raw[:8])
		} else {
			length = uint64(binary.NativeEndian.Uint32(raw[:4]))
		}
		if length < uint64(header) || length > uint64(len(raw)) {
			return packetInfo{}, fmt.Errorf("udpdiscovery: invalid control length %d", length)
		}
		level := binary.NativeEndian.Uint32(raw[layout.lengthBytes:])
		kind := binary.NativeEndian.Uint32(raw[layout.lengthBytes+4:])
		if level == 0 && kind == layout.packetType {
			if found || length != uint64(header+layout.payloadSize) {
				return packetInfo{}, fmt.Errorf("udpdiscovery: invalid packet-info control")
			}
			data := raw[header:int(length)]
			index := binary.NativeEndian.Uint32(data[layout.indexOffset:])
			if index == 0 || uint64(index) > uint64(^uint(0)>>1) {
				return packetInfo{}, fmt.Errorf("udpdiscovery: invalid packet-info interface")
			}
			var address [4]byte
			copy(address[:], data[layout.dstOffset:layout.dstOffset+4])
			result = packetInfo{dst: netip.AddrFrom4(address), index: int(index)}
			if result.dst.IsUnspecified() {
				return packetInfo{}, fmt.Errorf("udpdiscovery: missing packet-info destination")
			}
			found = true
		}
		if int(length) == len(raw) {
			break
		}
		next := layout.align(int(length))
		if next > len(raw) {
			return packetInfo{}, fmt.Errorf("udpdiscovery: truncated control alignment")
		}
		raw = raw[next:]
	}
	if !found {
		return packetInfo{}, fmt.Errorf("udpdiscovery: missing packet-info control")
	}
	return result, nil
}

func marshalPacketInfo(source netip.Addr, index int) []byte {
	return marshalControl(layout, source, index)
}

func marshalControl(layout controlLayout, source netip.Addr, index int) []byte {
	length := layout.headerSize() + layout.payloadSize
	raw := make([]byte, layout.align(length))
	if layout.lengthBytes == 8 {
		binary.NativeEndian.PutUint64(raw, uint64(length))
	} else {
		binary.NativeEndian.PutUint32(raw, uint32(length))
	}
	binary.NativeEndian.PutUint32(raw[layout.lengthBytes+4:], layout.packetType)
	data := raw[layout.headerSize():length]
	binary.NativeEndian.PutUint32(data[layout.indexOffset:], uint32(index))
	ip := source.As4()
	copy(data[layout.srcOffset:layout.srcOffset+4], ip[:])
	return raw
}
