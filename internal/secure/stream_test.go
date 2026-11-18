package secure

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"remote-enet/internal/transport"
)

func streamPair(t *testing.T, a, b transport.Session) (transport.Stream, transport.Stream) {
	t.Helper()
	ctx := deadline(t)
	x, err := a.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	y, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { x.Close(); y.Close() })
	return x, y
}

func awaitError(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(time.Second):
		t.Fatal("I/O did not unblock")
		return nil
	}
}

func assertStreamUsable(t *testing.T, x, y transport.Stream) {
	t.Helper()
	for _, ends := range [][2]transport.Stream{{x, y}, {y, x}} {
		ch := make(chan error, 1)
		go func() { _, err := ends[0].Write([]byte("ok")); ch <- err }()
		var buf [2]byte
		if _, err := io.ReadFull(ends[1], buf[:]); err != nil {
			t.Fatal(err)
		}
		if string(buf[:]) != "ok" {
			t.Fatal("stream payload changed")
		}
		if err := awaitError(t, ch); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStreamCloseUnblocksReadWrite(t *testing.T) {
	a, b := pair(t, false)
	x, _ := streamPair(t, a, b)
	activeX, activeY := streamPair(t, a, b)
	// Exhaust the default yamux send window; the peer deliberately never reads
	// this stream or sends FIN. Its mux continues serving other streams.
	if _, err := x.Write(make([]byte, 256*1024)); err != nil {
		t.Fatal(err)
	}
	readDone, writeDone := make(chan error, 1), make(chan error, 1)
	go func() { _, err := x.Read(make([]byte, 1)); readDone <- err }()
	go func() { _, err := x.Write([]byte("blocked")); writeDone <- err }()
	for _, ch := range []<-chan error{readDone, writeDone} {
		select {
		case err := <-ch:
			t.Fatalf("I/O returned before Close: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- x.Close() }()
	if err := awaitError(t, closeDone); err != nil {
		t.Fatal(err)
	}
	if err := awaitError(t, readDone); err == nil {
		t.Fatal("blocked read succeeded after Close")
	}
	if err := awaitError(t, writeDone); err == nil {
		t.Fatal("blocked write succeeded after Close")
	}
	if _, err := x.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if _, err := x.Write([]byte("closed")); !errors.Is(err, net.ErrClosed) {
		t.Fatal(err)
	}
	if err := x.Close(); err != nil {
		t.Fatal(err)
	}
	assertStreamUsable(t, activeX, activeY)
}

func TestCopyCancellationIsStreamLocal(t *testing.T) {
	a, b := pair(t, false)
	x, _ := streamPair(t, a, b)
	activeX, activeY := streamPair(t, a, b)
	local, remote := net.Pipe()
	defer remote.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stop := context.AfterFunc(ctx, func() { x.Close(); local.Close() })
	defer stop()
	copies := make(chan error, 2)
	go func() { _, err := io.Copy(local, x); copies <- err }()
	go func() { _, err := io.Copy(x, local); copies <- err }()
	cancel()
	awaitError(t, copies)
	awaitError(t, copies)
	assertStreamUsable(t, activeX, activeY)
}

func TestCloseWriteAllowsReply(t *testing.T) {
	a, b := pair(t, false)
	x, y := streamPair(t, a, b)
	if err := x.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := y.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := y.Write([]byte("reply"))
		if err == nil {
			err = y.CloseWrite()
		}
		done <- err
	}()
	data, err := io.ReadAll(x)
	if err != nil || string(data) != "reply" {
		t.Fatalf("reply=%q, err=%v", data, err)
	}
	if err := awaitError(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestOpenCancellationDisposesLateStream(t *testing.T) {
	a, b := pair(t, false)
	ctx := deadline(t)
	// Fill yamux's unacknowledged SYN quota without accepting on the peer.
	for range 256 {
		if _, err := a.OpenStream(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s := a.(*session)
	openCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := s.OpenStream(openCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if len(s.openGate) != 1 {
		t.Fatal("expected one pending yamux opener")
	}
	// Accepting releases a SYN slot. The cancelled request's late stream must
	// be closed rather than delivered to a caller that has already returned.
	first, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	select {
	case s.openGate <- struct{}{}:
		<-s.openGate
	case <-ctx.Done():
		t.Fatal("late opener retained gate")
	}
	for range 255 {
		st, err := b.AcceptStream(ctx)
		if err != nil {
			t.Fatal(err)
		}
		st.Close()
	}
	late, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer late.Close()
	if _, err := late.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("late stream not closed: %v", err)
	}
	x, y := streamPair(t, a, b)
	assertStreamUsable(t, x, y)
}

func TestSessionCloseReleasesCancelledOpener(t *testing.T) {
	a, _ := pair(t, false)
	ctx := deadline(t)
	for range 256 {
		if _, err := a.OpenStream(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s := a.(*session)
	openCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, err := s.OpenStream(openCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if err := s.Close(nil); err != nil {
		t.Fatal(err)
	}
	select {
	case s.openGate <- struct{}{}:
		<-s.openGate
	case <-ctx.Done():
		t.Fatal("session close did not release cancelled opener")
	}
}
