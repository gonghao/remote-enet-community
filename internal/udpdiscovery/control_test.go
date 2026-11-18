package udpdiscovery

import (
	"bytes"
	"encoding/binary"
	"net/netip"
	"testing"
)

func putControlLength(l controlLayout, raw []byte, n uint64) {
	if l.lengthBytes == 8 {
		binary.NativeEndian.PutUint64(raw, n)
	} else {
		binary.NativeEndian.PutUint32(raw, uint32(n))
	}
}

func TestNativeControlCodecLayouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		l    controlLayout
		data []byte
	}{
		{"linux64", controlLayout{8, 8, 8, 12, 0, 8, 4}, []byte{7, 0, 0, 0, 127, 0, 0, 1, 127, 255, 255, 255}},
		{"linux32", controlLayout{4, 4, 8, 12, 0, 8, 4}, []byte{7, 0, 0, 0, 127, 0, 0, 1, 127, 255, 255, 255}},
		{"darwin", controlLayout{4, 4, 26, 12, 0, 8, 4}, []byte{7, 0, 0, 0, 127, 0, 0, 1, 127, 255, 255, 255}},
		{"windows64", controlLayout{8, 8, 19, 8, 4, 0, 0}, []byte{127, 255, 255, 255, 7, 0, 0, 0}},
		{"windows32", controlLayout{4, 4, 19, 8, 4, 0, 0}, []byte{127, 255, 255, 255, 7, 0, 0, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tc.l
			length := l.headerSize() + len(tc.data)
			raw := make([]byte, l.align(length))
			putControlLength(l, raw, uint64(length))
			binary.NativeEndian.PutUint32(raw[l.lengthBytes+4:], l.packetType)
			copy(raw[l.headerSize():], tc.data)
			// Fixtures retain network-order addresses; the native interface integer
			// differs from the byte fixture on big-endian machines.
			binary.NativeEndian.PutUint32(raw[l.headerSize()+l.indexOffset:], 7)
			info, err := parseControl(l, raw)
			if err != nil || info.index != 7 || info.dst != netip.MustParseAddr("127.255.255.255") {
				t.Fatalf("parse packet info: %+v %v", info, err)
			}
			withoutPadding := raw[:length]
			if _, err := parseControl(l, withoutPadding); err != nil {
				t.Fatalf("final control may omit alignment padding: %v", err)
			}
			src := netip.MustParseAddr("127.0.0.2")
			out := marshalControl(l, src, 9)
			data := out[l.headerSize():]
			if !bytes.Equal(data[l.srcOffset:l.srcOffset+4], []byte{127, 0, 0, 2}) || binary.NativeEndian.Uint32(data[l.indexOffset:]) != 9 || len(out) != l.align(length) {
				t.Fatalf("reply control did not pin source/interface: %x", out)
			}
			unknown := bytes.Clone(raw)
			binary.NativeEndian.PutUint32(unknown[l.lengthBytes+4:], 999)
			if _, err := parseControl(l, append(unknown, raw...)); err != nil {
				t.Fatalf("valid unknown control before packet info: %v", err)
			}
			badLength := bytes.Clone(raw)
			putControlLength(l, badLength, uint64(len(raw)+1))
			shortLength := bytes.Clone(raw)
			putControlLength(l, shortLength, uint64(l.headerSize()-1))
			zeroIndex := bytes.Clone(raw)
			binary.NativeEndian.PutUint32(zeroIndex[l.headerSize()+l.indexOffset:], 0)
			zeroAddress := bytes.Clone(raw)
			clear(zeroAddress[l.headerSize()+l.dstOffset : l.headerSize()+l.dstOffset+4])
			for _, bad := range [][]byte{nil, {0}, raw[:l.headerSize()-1], raw[:length-1], badLength, shortLength, zeroIndex, zeroAddress, unknown, append(bytes.Clone(raw), raw...), append(bytes.Clone(raw), 1)} {
				if _, err := parseControl(l, bad); err == nil {
					t.Errorf("malformed/missing metadata accepted: %x", bad)
				}
			}
		})
	}
}

func TestNativeDestinationAndInterfaceFilter(t *testing.T) {
	e := endpoint{bind: netip.MustParseAddr("127.0.0.1"), index: 7, bcast: netip.MustParseAddr("127.255.255.255")}
	for _, tc := range []struct {
		dst   string
		index int
		want  bool
	}{{"127.0.0.1", 7, true}, {"127.255.255.255", 7, true}, {"255.255.255.255", 7, true}, {"127.0.0.2", 7, false}, {"127.0.0.1", 8, false}, {"224.0.0.1", 7, false}, {"0.0.0.0", 7, false}} {
		if got := e.accepts(packetInfo{dst: netip.MustParseAddr(tc.dst), index: tc.index}); got != tc.want {
			t.Errorf("filter %s/index=%d: %t", tc.dst, tc.index, got)
		}
	}
}
