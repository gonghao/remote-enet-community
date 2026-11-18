//go:build linux

package udpdiscovery

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestBroadcastFixedSourceAndDestinationFilter(t *testing.T) {
	c, err := Listen(t.Context(), netip.MustParseAddr("127.0.0.2"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	tool := nativeTool(t)
	address := c.LocalAddr()
	wrong := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: address.Port}
	broadcast := &net.UDPAddr{IP: net.IPv4(127, 255, 255, 255), Port: address.Port}
	if _, err := tool.WriteToUDP([]byte("wrong destination"), wrong); err != nil {
		t.Fatal(err)
	}
	want := []byte{0, 0xff, 0x11, 7}
	if _, err := tool.WriteToUDP(want, broadcast); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	var b [128]byte
	n, peer, err := c.Read(ctx, b[:])
	if err != nil || !bytes.Equal(b[:n], want) {
		t.Fatalf("broadcast/filter: n=%d payload=%q err=%v", n, b[:n], err)
	}
	if _, err := c.Write(ctx, want, peer); err != nil {
		t.Fatal(err)
	}
	n, from, err := tool.ReadFromUDP(b[:])
	if err != nil || from.String() != address.String() || !bytes.Equal(b[:n], want) {
		t.Fatalf("broadcast reply source: from=%v expected=%v n=%d err=%v", from, address, n, err)
	}
}
