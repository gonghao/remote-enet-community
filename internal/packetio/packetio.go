// Package packetio preserves IP packet boundaries across native and memory devices.
package packetio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
)

// Device copies complete packets. Individual canceled operations leave the
// device usable; Close unblocks all calls. A short read buffer drops that packet
// and returns io.ErrShortBuffer, rather than injecting a truncated IP packet.
type Device interface {
	ReadPacket(context.Context, []byte) (int, error)
	WritePacket(context.Context, []byte) (int, error)
	MTU() int
	Close() error
}

type pair struct {
	done chan struct{}
	once sync.Once
}

type memory struct {
	link      *pair
	mtu       int
	in, out   chan []byte
	writeGate chan struct{}
}

// NewPair bounds both queues; closing either endpoint closes the entire link.
func NewPair(mtu, capacity int) (Device, Device, error) {
	if mtu < 576 || mtu > 65535 || capacity < 1 || capacity > 1024 {
		return nil, nil, fmt.Errorf("packetio: invalid MTU or queue capacity")
	}
	link := &pair{done: make(chan struct{})}
	a, b := make(chan []byte, capacity), make(chan []byte, capacity)
	return &memory{link: link, mtu: mtu, in: a, out: b, writeGate: make(chan struct{}, 1)},
		&memory{link: link, mtu: mtu, in: b, out: a, writeGate: make(chan struct{}, 1)}, nil
}

func (d *memory) MTU() int { return d.mtu }

func (d *memory) ReadPacket(ctx context.Context, buffer []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-d.link.done:
		return 0, io.EOF
	default:
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.link.done:
		return 0, io.EOF
	case packet := <-d.in:
		if len(buffer) < len(packet) {
			return 0, io.ErrShortBuffer
		}
		return copy(buffer, packet), nil
	}
}

func (d *memory) WritePacket(ctx context.Context, packet []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-d.link.done:
		return 0, io.ErrClosedPipe
	default:
	}
	if len(packet) == 0 || len(packet) > d.mtu {
		return 0, fmt.Errorf("packetio: packet length %d outside 1..%d", len(packet), d.mtu)
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.link.done:
		return 0, io.ErrClosedPipe
	case d.writeGate <- struct{}{}:
	}
	defer func() { <-d.writeGate }()
	owned := bytes.Clone(packet)
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.link.done:
		return 0, io.ErrClosedPipe
	case d.out <- owned:
		return len(packet), nil
	}
}

func (d *memory) Close() error {
	d.link.once.Do(func() { close(d.link.done) })
	return nil
}
