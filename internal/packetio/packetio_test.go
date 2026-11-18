package packetio

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func pairForTest(t *testing.T, capacity int) (Device, Device) {
	t.Helper()
	a, b, err := NewPair(1500, capacity)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
}

func TestPacketBoundariesAndBufferOwnership(t *testing.T) {
	a, b := pairForTest(t, 2)
	first := []byte{1, 2, 3}
	if _, err := a.WritePacket(t.Context(), first); err != nil {
		t.Fatal(err)
	}
	first[0] = 99
	if _, err := a.WritePacket(t.Context(), []byte{4}); err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, b.MTU())
	n, err := b.ReadPacket(t.Context(), buffer)
	if err != nil || !bytes.Equal(buffer[:n], []byte{1, 2, 3}) {
		t.Fatalf("first packet: %v, %v", buffer[:n], err)
	}
	n, err = b.ReadPacket(t.Context(), buffer)
	if err != nil || !bytes.Equal(buffer[:n], []byte{4}) {
		t.Fatalf("second packet: %v, %v", buffer[:n], err)
	}
	if _, err := b.WritePacket(t.Context(), []byte{5, 6}); err != nil {
		t.Fatal(err)
	}
	n, err = a.ReadPacket(t.Context(), buffer)
	if err != nil || !bytes.Equal(buffer[:n], []byte{5, 6}) {
		t.Fatalf("reverse packet: %v, %v", buffer[:n], err)
	}
}

func TestShortBufferDropsWholePacket(t *testing.T) {
	a, b := pairForTest(t, 2)
	_, _ = a.WritePacket(t.Context(), []byte{1, 2, 3})
	if n, err := b.ReadPacket(t.Context(), make([]byte, 1)); n != 0 || !errors.Is(err, io.ErrShortBuffer) {
		t.Fatalf("short buffer returned %d, %v", n, err)
	}
	_, _ = a.WritePacket(t.Context(), []byte{7})
	buffer := make([]byte, 1)
	if n, err := b.ReadPacket(t.Context(), buffer); err != nil || n != 1 || buffer[0] != 7 {
		t.Fatalf("truncated tail was retained: %v, %d, %v", buffer, n, err)
	}
}

func TestCanceledOperationsKeepDeviceUsable(t *testing.T) {
	a, b := pairForTest(t, 1)
	_, _ = a.WritePacket(t.Context(), []byte{1})
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if _, err := a.WritePacket(ctx, []byte{2}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write: %v", err)
	}
	readCtx, readCancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer readCancel()
	buffer := make([]byte, 1500)
	if _, err := a.ReadPacket(readCtx, buffer); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked read: %v", err)
	}
	if _, err := b.WritePacket(t.Context(), []byte{3}); err != nil {
		t.Fatal(err)
	}
	if n, err := a.ReadPacket(t.Context(), buffer); err != nil || n != 1 || buffer[0] != 3 {
		t.Fatalf("reverse direction lost after cancellation: %d, %v", n, err)
	}
	if n, err := b.ReadPacket(t.Context(), buffer); err != nil || n != 1 || buffer[0] != 1 {
		t.Fatalf("queued packet lost: %d, %v", n, err)
	}
	if _, err := a.WritePacket(t.Context(), []byte{4}); err != nil {
		t.Fatalf("write queue was not released: %v", err)
	}
}

func TestCloseUnblocksBothDirections(t *testing.T) {
	a, b := pairForTest(t, 1)
	_, _ = a.WritePacket(t.Context(), []byte{1})
	read, write := make(chan error, 1), make(chan error, 1)
	go func() { _, err := a.ReadPacket(t.Context(), make([]byte, 1500)); read <- err }()
	go func() { _, err := a.WritePacket(t.Context(), []byte{2}); write <- err }()
	_ = b.Close()
	for label, result := range map[string]<-chan error{"read": read, "write": write} {
		select {
		case err := <-result:
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("%s close result: %v", label, err)
			}
		case <-time.After(time.Second):
			t.Fatalf("%s remained blocked", label)
		}
	}
	for range 100 {
		if _, err := a.WritePacket(t.Context(), []byte{9}); !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("closed link accepted a packet: %v", err)
		}
	}
	_ = a.Close()
}

func TestLimits(t *testing.T) {
	for _, cfg := range [][2]int{{575, 1}, {65536, 1}, {1500, 0}, {1500, 1025}} {
		if _, _, err := NewPair(cfg[0], cfg[1]); err == nil {
			t.Fatalf("invalid configuration accepted: %v", cfg)
		}
	}
	a, _ := pairForTest(t, 1)
	for _, packet := range [][]byte{nil, make([]byte, 1501)} {
		if _, err := a.WritePacket(t.Context(), packet); err == nil {
			t.Fatal("invalid packet accepted")
		}
	}
}
