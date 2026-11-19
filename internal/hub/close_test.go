package hub

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

type delayedListenerClose struct {
	accepted chan struct{}
	closing  chan struct{}
	release  chan struct{}
	finished chan struct{}
	started  atomic.Bool
}

func (l *delayedListenerClose) Accept() (net.Conn, error) {
	close(l.accepted)
	<-l.closing
	return nil, net.ErrClosed
}

func (l *delayedListenerClose) Close() error {
	if l.started.CompareAndSwap(false, true) {
		close(l.closing)
		<-l.release
		close(l.finished)
	}
	return nil
}

func (*delayedListenerClose) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1}
}

func TestServeJoinsListenerClose(t *testing.T) {
	id := local(t)
	synctest.Test(t, func(t *testing.T) {
		listener := &delayedListenerClose{accepted: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{}), finished: make(chan struct{})}
		var once sync.Once
		release := func() { once.Do(func() { close(listener.release) }) }
		defer release()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- NewServer(Options{Local: id, Policy: testPolicy{}}).Serve(ctx, listener) }()
		<-listener.accepted
		cancel()
		<-listener.closing
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("Serve returned before listener close finished: %v", err)
		default:
		}
		release()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		select {
		case <-listener.finished:
		default:
			t.Fatal("listener close was not joined")
		}
	})
}
