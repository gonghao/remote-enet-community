package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"remote-enet/internal/identity"
)

// Options callbacks can run concurrently and must return promptly.
type Options struct {
	Local     *identity.Local
	Authorize func(dev identity.Fingerprint, role Role, target identity.Fingerprint) bool
	// AuthorizeToken optionally validates standalone-hub credentials. Embedded
	// self-dials ignore Token. Authorize still applies to every request.
	AuthorizeToken func(dev identity.Fingerprint, role Role, target identity.Fingerprint, token []byte) bool
	Embedded       *Embedded
	Logger         *slog.Logger
	// SetupLimits applies only when Embedded is enabled.
	SetupLimits SetupLimits
}

// Server owns its listener and all accepted connections until Serve returns.
// A server can be served only once, including after shutdown.
type Server struct {
	o       Options
	started atomic.Bool
	mu      sync.Mutex
	waits   map[identity.Fingerprint]*waiter
}

type waiter struct {
	conn   *connection
	target identity.Fingerprint
	reader *io.PipeReader
	ready  chan struct{}
	busy   bool // guarded by Server.mu, including the pre-pair read check
}

// connection closes the socket directly: TLS close_notify must not stall shutdown.
type connection struct {
	net.Conn
	raw          net.Conn
	once         sync.Once
	done         chan struct{}
	releaseSetup func()
}

func (c *connection) Close() error {
	var err error
	c.once.Do(func() {
		err = c.raw.Close()
		close(c.done)
	})
	return err
}

func (c *connection) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

func (c *connection) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		_ = c.Close()
	}
	return n, err
}

// Only the setup worker touches releaseSetup. Long-lived relay and wait
// connections release their slot before entering the forwarding loop.
func (c *connection) releaseSlot() {
	if c.releaseSetup != nil {
		c.releaseSetup()
		c.releaseSetup = nil
	}
}

func (c *connection) finishSetup() {
	_ = c.SetDeadline(time.Time{})
	c.releaseSlot()
}

// NewServer leaves listener ownership and startup to Serve.
func NewServer(o Options) *Server {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	return &Server{o: o, waits: make(map[identity.Fingerprint]*waiter)}
}

// Serve closes ln and joins all connection handlers before returning.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("hub: Serve called more than once")
	}
	defer ln.Close()
	if s.o.Local == nil {
		return errors.New("hub: local identity required")
	}
	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	closed := make(chan struct{})
	stop := context.AfterFunc(serveCtx, func() { _ = ln.Close(); close(closed) })
	defer func() {
		// Waking Accept is not proof that Close released the descriptor. Join
		// our callback before the final synchronous close and returning ownership.
		if !stop() {
			<-closed
		}
	}()
	var wg sync.WaitGroup
	var connsMu sync.Mutex
	conns := make(map[*connection]struct{})
	defer func() {
		cancel()
		connsMu.Lock()
		for conn := range conns {
			_ = conn.Close()
		}
		connsMu.Unlock()
		wg.Wait()
	}()
	var admission *setupAdmission
	timeout := setupTimeout
	if s.o.Embedded != nil {
		admission = newSetupAdmission(s.o.SetupLimits)
		timeout = admission.Timeout
	}
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("hub: accept: %w", err)
		}
		now := time.Now()
		var release func()
		if admission != nil {
			if !admission.acquire(now) {
				_ = raw.Close()
				continue
			}
			release = admission.release
		}
		_ = raw.SetDeadline(now.Add(timeout))
		conn := &connection{releaseSetup: release, Conn: tls.Server(raw, serverTLS(s.o.Local)), raw: raw, done: make(chan struct{})}
		connsMu.Lock()
		conns[conn] = struct{}{}
		connsMu.Unlock()
		wg.Go(func() {
			// Embedded handoff retains this lifetime monitor until its owner closes.
			defer func() {
				_ = conn.Close()
				connsMu.Lock()
				delete(conns, conn)
				connsMu.Unlock()
			}()
			if s.handle(serveCtx, conn, timeout) {
				<-conn.done
			}
		})
	}
}

func (s *Server) handle(ctx context.Context, conn *connection, timeout time.Duration) bool {
	defer conn.releaseSlot()
	tc := conn.Conn.(*tls.Conn)
	if err := tc.HandshakeContext(ctx); err != nil {
		s.o.Logger.DebugContext(ctx, "Hub TLS handshake failed", "error", err)
		return false
	}
	dev, err := identity.FingerprintOfCert(tc.ConnectionState().PeerCertificates[0])
	if err != nil {
		panic(err) // verifyPeer already established this invariant.
	}
	req, err := readRequest(conn)
	if err != nil {
		return false
	}
	embedded := s.o.Embedded != nil && req.Role == RoleDial && req.Target == s.o.Local.Fingerprint()
	if (s.o.Authorize != nil && !s.o.Authorize(dev, req.Role, req.Target)) ||
		(!embedded && s.o.AuthorizeToken != nil && !s.o.AuthorizeToken(dev, req.Role, req.Target, req.Token)) {
		_ = writeAll(conn, []byte{statusUnauthorized})
		return false
	}
	if embedded {
		if !s.o.Embedded.reserve(conn, timeout) {
			_ = writeAll(conn, []byte{statusBusy})
			return false
		}
		if err := writeAll(conn, []byte{statusOK}); err != nil {
			return false
		}
		conn.finishSetup()
		return s.o.Embedded.publish(conn, dev)
	}
	if req.Role == RoleWait {
		s.wait(conn, dev, req.Target)
	} else {
		s.dial(conn, dev, req.Target)
	}
	return false
}

func (s *Server) wait(conn *connection, dev, target identity.Fingerprint) {
	r, w := io.Pipe()
	wait := &waiter{conn: conn, target: target, reader: r, ready: make(chan struct{})}
	s.mu.Lock()
	if _, exists := s.waits[dev]; exists {
		s.mu.Unlock()
		_ = r.Close()
		_ = w.Close()
		_ = writeAll(conn, []byte{statusBusy})
		return
	}
	s.waits[dev] = wait
	s.mu.Unlock()
	defer func() {
		_ = conn.Close()
		_ = r.Close()
		_ = w.Close()
		s.mu.Lock()
		delete(s.waits, dev)
		s.mu.Unlock()
	}()
	err := writeAll(conn, []byte{statusOK})
	conn.finishSetup()
	// Dial can reserve immediately, but cannot write to the wait socket until
	// its routing response is complete. Never hold the routing lock during I/O.
	close(wait.ready)
	if err != nil {
		return
	}
	// Only this handler reads the wait socket. Before pairing, waiters must be
	// silent; rejecting eager data avoids an unbounded pre-pair buffer.
	buf := make([]byte, 32*1024)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			s.mu.Lock()
			paired := wait.busy
			s.mu.Unlock()
			if !paired {
				return
			}
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) dial(conn *connection, dev, target identity.Fingerprint) {
	s.mu.Lock()
	wait := s.waits[target]
	status := statusOK
	switch {
	case wait == nil:
		status = statusUnknownPeer
	case wait.busy:
		status = statusBusy
	case !wait.target.IsZero() && wait.target != dev:
		status = statusUnauthorized
	default:
		wait.busy = true
	}
	s.mu.Unlock()
	if status != statusOK {
		_ = writeAll(conn, []byte{status})
		return
	}
	defer wait.conn.Close()
	defer wait.reader.Close()
	<-wait.ready
	if err := writeAll(conn, []byte{statusOK}); err != nil {
		return
	}
	conn.finishSetup()
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(wait.conn, conn)
		_ = wait.conn.Close()
		_ = wait.reader.Close()
		_ = conn.Close()
		close(done)
	}()
	_, _ = io.Copy(conn, wait.reader)
	_ = conn.Close()
	_ = wait.conn.Close()
	_ = wait.reader.Close()
	<-done
}

type accepted struct {
	conn *connection
	peer identity.Fingerprint
}

// Embedded transfers connection ownership to Accept's caller. The active slot
// remains busy until that connection is closed (also on server shutdown).
type Embedded struct {
	mu      sync.Mutex
	active  *connection
	pending *accepted
	claimed bool
	timer   *time.Timer
	notify  chan struct{}
}

// NewEmbedded provides one admission slot shared by all servers using it.
func NewEmbedded() *Embedded { return &Embedded{notify: make(chan struct{}, 1)} }

func (e *Embedded) reserve(conn *connection, timeout time.Duration) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != nil {
		select {
		case <-e.active.done:
		default:
			return false
		}
	}
	if e.timer != nil {
		e.timer.Stop()
	}
	e.active = conn
	e.pending = nil
	e.claimed = false
	// Pending handoffs have no reader yet, so socket deadlines alone cannot
	// detect disconnects or bound slot occupancy. Expire them explicitly.
	e.timer = time.AfterFunc(timeout, func() {
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.active == conn && !e.claimed {
			e.pending = nil
			_ = conn.Close()
		}
	})
	return true
}

func (e *Embedded) publish(conn *connection, peer identity.Fingerprint) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.active != conn {
		return false
	}
	select {
	case <-conn.done:
		return false
	default:
	}
	e.pending = &accepted{conn: conn, peer: peer}
	select {
	case e.notify <- struct{}{}:
	default:
	}
	return true
}

// Accept transfers ownership of the connection; its caller must close it.
func (e *Embedded) Accept(ctx context.Context) (net.Conn, identity.Fingerprint, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, identity.Fingerprint{}, err
		}
		e.mu.Lock()
		next := e.pending
		if next != nil {
			e.pending = nil
			e.claimed = true
			e.timer.Stop()
		}
		e.mu.Unlock()
		if next != nil {
			select {
			case <-next.conn.done:
				continue
			default:
				return next.conn, next.peer, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, identity.Fingerprint{}, ctx.Err()
		case <-e.notify:
		}
	}
}
