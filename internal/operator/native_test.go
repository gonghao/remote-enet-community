package operator

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

// Native socket tests bind only the assigned 127.0.0.1 address. They run on
// Windows and macOS CI without relying on Linux's implicit 127/8 aliases.
func TestNativeDiscoveryEndpointLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	opener := openFunc(func(context.Context, proto.OpenTunnel) (transport.Stream, error) {
		t.Error("discovery opened a TCP tunnel")
		return nil, errors.New("unexpected data tunnel")
	})
	p, err := Start(ctx, Options{Bind: netip.MustParseAddr("127.0.0.1"), Opener: opener})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	diag, ctrl, disc := p.Addr()
	tool, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()
	probe(t, tool, disc, hsfz.IdentRequest, nil)
	state := testState(t, "127.0.0.2")
	want := bytes.Clone(state.IdentRaw)
	if err := p.Update(state); err != nil {
		t.Fatal(err)
	}
	state.IdentRaw[6] ^= 0xff
	probe(t, tool, disc, hsfz.IdentRequest, want)
	probe(t, tool, disc, append(bytes.Clone(hsfz.IdentRequest), 0), nil)
	probe(t, tool, disc, []byte{0, 0, 0, 0, 0, 0x10}, nil)
	if err := p.Update(proto.VehicleState{}); err != nil {
		t.Fatal(err)
	}
	probe(t, tool, disc, hsfz.IdentRequest, nil)
	cancel()
	closed := make(chan error, 1)
	go func() { closed <- p.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("presenter cancellation did not release its workers")
	}
	if err := p.Update(proto.VehicleState{}); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("update after Close: %v", err)
	}
	for _, addr := range []*net.TCPAddr{diag, ctrl} {
		listener, err := net.ListenTCP("tcp4", addr)
		if err != nil {
			t.Fatalf("TCP listener not released: %s: %v", addr, err)
		}
		listener.Close()
	}
	socket, err := net.ListenUDP("udp4", disc)
	if err != nil {
		t.Fatalf("UDP endpoint not released: %s: %v", disc, err)
	}
	socket.Close()
}
