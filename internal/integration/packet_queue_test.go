package integration_test

import (
	"testing"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

func TestFixtureClientQueueHoldsTCPWindowBurst(t *testing.T) {
	link := newObservedPacketLink()
	defer link.Close()
	// One maximum TCP send window can be handed to the NIC before its separate
	// packet worker runs. Baseline loss must come only from explicit injectors.
	const payload = 1440
	count := (tcp.MaxBufferSize + payload - 1) / payload
	var packets stack.PacketBufferList
	for range count {
		packets.PushBack(stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(make([]byte, 1500))}))
	}
	defer func() {
		for _, packet := range packets.AsSlice() {
			packet.DecRef()
		}
		link.Drain()
	}()
	n, err := link.WritePackets(packets)
	if err != nil || n != count || link.shortPackets.Load() != 0 {
		t.Fatalf("unconfigured queue loss: accepted=%d/%d short=%d err=%v", n, count, link.shortPackets.Load(), err)
	}
}
