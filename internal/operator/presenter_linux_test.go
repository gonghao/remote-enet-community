//go:build linux

package operator

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

func TestDiscoveryDirectedBroadcast(t *testing.T) {
	state := testState(t, "127.0.0.2")
	p := startPresenter(t, Options{State: state, Opener: openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		t.Error("broadcast discovery opened a TCP tunnel")
		return nil, errors.New("unexpected data tunnel")
	})})
	_, _, disc := p.Addr()
	tool := udpClient(t)
	if err := tool.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	broadcast := &net.UDPAddr{IP: net.IPv4(127, 255, 255, 255), Port: disc.Port}
	if _, err := tool.WriteToUDP(hsfz.IdentRequest, broadcast); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 65535)
	n, source, err := tool.ReadFromUDP(buffer)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(buffer[:n], state.IdentRaw) || !source.IP.Equal(disc.IP) || source.Port != disc.Port {
		t.Fatalf("broadcast response changed bytes or source: source=%v bytes=%x", source, buffer[:n])
	}
	probe(t, tool, broadcast, append(bytes.Clone(hsfz.IdentRequest), 0), nil)
	// A wildcard listener must not expose this gateway through other local
	// addresses just because they use the same loopback interface.
	other := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: disc.Port}
	probe(t, tool, other, hsfz.IdentRequest, nil)
	probe(t, tool, disc, hsfz.IdentRequest, state.IdentRaw)
}
