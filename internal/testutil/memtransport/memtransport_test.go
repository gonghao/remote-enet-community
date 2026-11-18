package memtransport

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"remote-enet/internal/identity"
)

func TestHalfCloseAndDone(t *testing.T) {
	a, b := Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	x, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer x.Close()
	y, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer y.Close()
	if _, err = x.Write([]byte("req")); err != nil {
		t.Fatal(err)
	}
	if err = x.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(y)
	if err != nil || string(data) != "req" {
		t.Fatalf("%q %v", data, err)
	}
	if _, err = y.Write([]byte("res")); err != nil {
		t.Fatal(err)
	}
	if err = y.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	data, err = io.ReadAll(x)
	if err != nil || string(data) != "res" {
		t.Fatalf("%q %v", data, err)
	}
	if a.Peer() != (identity.Fingerprint{2}) || b.Peer() != (identity.Fingerprint{1}) {
		t.Fatal("peer")
	}
	reason := errors.New("closed")
	if err := a.Close(reason); err != nil {
		t.Fatal(err)
	}
	for _, done := range []<-chan struct{}{a.Done(), b.Done()} {
		select {
		case <-done:
		case <-ctx.Done():
			t.Fatal("Done not propagated")
		}
	}
	if !errors.Is(a.Err(), reason) || b.Err() == nil {
		t.Fatalf("%v %v", a.Err(), b.Err())
	}
}
func TestContextAndFullClose(t *testing.T) {
	a, b := Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.OpenStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if _, err := b.AcceptStream(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	x, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := x.Read(make([]byte, 1)); done <- err }()
	_ = x.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read succeeded")
		}
	case <-ctx.Done():
		t.Fatal("Close did not unblock Read")
	}
}

func TestPendingOpenCancellationKeepsExistingStream(t *testing.T) {
	a, b := Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	x, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Fill yamux's pending-SYN limit without accepting any further streams.
	for i := 0; i < 256; i++ {
		if _, err := a.OpenStream(ctx); err != nil {
			t.Fatal(err)
		}
	}
	pending, cancelPending := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelPending()
	if _, err := a.OpenStream(pending); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
		t.Fatal("pending open cancellation closed the transport")
	default:
	}
	if _, err := x.Write([]byte("alive")); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 5)
	if _, err := io.ReadFull(y, data); err != nil || string(data) != "alive" {
		t.Fatalf("existing stream: %q %v", data, err)
	}
}
