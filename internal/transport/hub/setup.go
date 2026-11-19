package hub

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

const innerSetupTimeout = 10 * time.Second

// setupConn bounds inner setup without giving Wrap a short-lived context that
// would cancel an otherwise healthy session. Waiters arm on first traffic.
type setupConn struct {
	net.Conn
	mu       sync.Mutex
	timer    *time.Timer
	finished bool
	started  bool
	cancel   context.CancelCauseFunc
}

func (c *setupConn) arm() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.started || c.finished {
		return
	}
	c.started = true
	_ = c.Conn.SetDeadline(time.Now().Add(innerSetupTimeout))
	c.timer = time.AfterFunc(innerSetupTimeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if !c.finished {
			c.cancel(context.DeadlineExceeded)
			_ = c.Conn.Close()
		}
	})
}

func (c *setupConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		c.arm()
	}
	return n, err
}

func (c *setupConn) Write(p []byte) (int, error) {
	c.arm()
	return c.Conn.Write(p)
}

func (c *setupConn) finish() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.finished = true
	if c.timer != nil {
		c.timer.Stop()
	}
	return c.Conn.SetDeadline(time.Time{})
}

func (a *Connector) wrap(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, p transport.PathInfo, idleWait bool) (transport.Session, error) {
	wrapCtx, cancel := context.WithCancelCause(ctx)
	c := &setupConn{Conn: conn, cancel: cancel}
	if !idleWait {
		c.arm()
	}
	stop := context.AfterFunc(wrapCtx, func() { _ = conn.Close() })
	session, err := a.o.Wrap(wrapCtx, c, isClient, expect, p)
	finishErr := c.finish()
	stop()
	if err == nil {
		err = finishErr
	}
	if wrapCtx.Err() != nil {
		err = context.Cause(wrapCtx)
	}
	if err != nil {
		cancel(err)
		_ = conn.Close()
		if session != nil {
			_ = session.Close(err)
		}
		return nil, fmt.Errorf("transport/hub: wrap: %w", err)
	}
	return ownSession(wrapCtx, session, func() { cancel(context.Canceled); _ = conn.Close() }), nil
}

// ownedSession joins lifetime cleanup on Close and Done, including spontaneous
// remote shutdown. All cleanup callbacks must be nonblocking and idempotent.
type ownedSession struct {
	transport.Session
	done chan struct{}
}

func ownSession(ctx context.Context, session transport.Session, cleanup func()) transport.Session {
	s := &ownedSession{Session: session, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = session.Close(ctx.Err())
		case <-session.Done():
		}
		cleanup()
		close(s.done)
	}()
	if exporter, ok := session.(interface{ PairingExporter() [32]byte }); ok {
		return &ownedPairingSession{ownedSession: s, exporter: exporter}
	}
	return s
}

type ownedPairingSession struct {
	*ownedSession
	exporter interface{ PairingExporter() [32]byte }
}

func (s *ownedPairingSession) PairingExporter() [32]byte { return s.exporter.PairingExporter() }

func (s *ownedSession) Done() <-chan struct{} { return s.done }

func (s *ownedSession) Close(reason error) error {
	err := s.Session.Close(reason)
	<-s.done
	return err
}
