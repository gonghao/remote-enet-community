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

// Grant carries immutable policy data across reloads. Expires is the inclusive
// admission deadline (exp + skew); zero means no ticket deadline. PairingUntil
// is inclusive and includes skew; zero keeps the pairing window closed.
type Grant struct {
	Ref                   any
	Expires, PairingUntil time.Time
}

// Policy snapshots must support concurrent Admit calls without external I/O.
// Errors returned by Admit must be safe to log (never include a raw ticket).
// Valid runs under the routing lock, including on grants from older snapshots.
// It checks revocation only, never time. The server checks Expires at commit.
// PairingOperator returns zero when the bootstrap route is disabled.
type Policy interface {
	Admit(dev identity.Fingerprint, req Request, now time.Time) (Grant, error)
	Valid(g Grant, dev identity.Fingerprint) bool
	PairingOperator() identity.Fingerprint
}

// Options.Now controls grant deadlines only; socket deadlines use real time.
type Options struct {
	Local       *identity.Local
	Policy      Policy
	Now         func() time.Time
	Embedded    *Embedded
	Logger      *slog.Logger
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
	dev       identity.Fingerprint
	conn      *connection
	target    identity.Fingerprint
	reader    *io.PipeReader
	ready     chan struct{}
	grant     Grant
	writer    *io.PipeWriter
	dial      *connection // guarded by Server.mu
	dialDev   identity.Fingerprint
	dialGrant Grant
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
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Server{o: o, waits: make(map[identity.Fingerprint]*waiter)}
}

// SetPolicy atomically replaces the policy and closes revoked routes. Counts
// distinguish idle waits from pairs. A nil policy revokes every relay route.
func (s *Server) SetPolicy(p Policy) (closedWaits, closedPairs int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.o.Policy = p
	for dev, w := range s.waits {
		if p != nil && p.Valid(w.grant, dev) && (w.dial == nil || p.Valid(w.dialGrant, w.dialDev)) {
			continue
		}
		s.removeWaitLocked(w)
		if w.dial == nil {
			closedWaits++
		} else {
			closedPairs++
		}
	}
	return
}

func (s *Server) policy() Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.o.Policy
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
	if s.o.Embedded == nil && s.policy() == nil {
		return errors.New("hub: standalone server requires a policy")
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
	admission := newSetupAdmission(s.o.SetupLimits, s.o.Embedded != nil)
	timeout := admission.Timeout
	for {
		raw, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("hub: accept: %w", err)
		}
		now := time.Now()
		if !admission.acquire(now) {
			_ = raw.Close()
			continue
		}
		_ = raw.SetDeadline(now.Add(timeout))
		conn := &connection{releaseSetup: admission.release, Conn: tls.Server(raw, serverTLS(s.o.Local)), raw: raw, done: make(chan struct{})}
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
	selfDial := req.Role == RoleDial && req.Target == s.o.Local.Fingerprint()
	embedded := selfDial && s.o.Embedded != nil
	p := s.policy()
	var grant Grant
	if (p == nil && !embedded) || (selfDial && !embedded && len(req.Token) != 0) {
		_ = writeAll(conn, []byte{statusUnauthorized})
		return false
	}
	if p != nil {
		grant, err = p.Admit(dev, req, s.o.Now())
		if err != nil {
			s.o.Logger.DebugContext(ctx, "Hub policy denied request", "device", dev.Short(), "error", err)
			_ = writeAll(conn, []byte{statusUnauthorized})
			return false
		}
	}
	wait, status := s.register(conn, dev, req, grant, p != nil, selfDial, timeout)
	if wait != nil && req.Role == RoleDial {
		select {
		case <-wait.ready:
		case <-wait.conn.done:
			s.removeWait(wait)
			return false
		}
	}
	if err := writeAll(conn, []byte{status}); err != nil || status != statusOK {
		if wait != nil {
			s.removeWait(wait)
		}
		return false
	}
	conn.finishSetup()
	switch {
	case embedded:
		return s.o.Embedded.publish(conn, dev)
	case req.Role == RoleWait:
		close(wait.ready)
		s.wait(wait)
	default:
		s.dial(conn, wait, timeout)
	}
	return false
}

// register commits the current policy check, slot reservation and both grants
// together. A rejected dial never consumes the wait or its pairing window.
func (s *Server) register(conn *connection, dev identity.Fingerprint, req Request, g Grant, admitted, selfDial bool, timeout time.Duration) (*waiter, byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.o.Policy
	if p != nil {
		if !admitted || !p.Valid(g, dev) {
			return nil, statusUnauthorized
		}
	} else if !selfDial || s.o.Embedded == nil {
		return nil, statusUnauthorized
	}
	now := s.o.Now()
	if !g.Expires.IsZero() && now.After(g.Expires) {
		return nil, statusUnauthorized
	}
	if selfDial && s.o.Embedded != nil {
		if !s.o.Embedded.reserve(conn, timeout) {
			return nil, statusBusy
		}
		return nil, statusOK
	}
	if req.Role == RoleWait {
		if s.waits[dev] != nil {
			return nil, statusBusy
		}
		r, w := io.Pipe()
		wait := &waiter{dev: dev, conn: conn, target: req.Target, grant: g, reader: r, writer: w, ready: make(chan struct{})}
		s.waits[dev] = wait
		return wait, statusOK
	}
	target := req.Target
	if selfDial {
		target = p.PairingOperator()
		if target.IsZero() {
			return nil, statusUnauthorized
		}
	}
	wait := s.waits[target]
	switch {
	case wait == nil:
		return nil, statusUnknownPeer
	case wait.dial != nil:
		return nil, statusBusy
	case selfDial && (wait.grant.PairingUntil.IsZero() || now.After(wait.grant.PairingUntil)):
		return nil, statusUnauthorized
	case !wait.target.IsZero() && wait.target != dev:
		return nil, statusUnauthorized
	}
	wait.dial, wait.dialDev, wait.dialGrant = conn, dev, g
	return wait, statusOK
}

func (s *Server) removeWait(wait *waiter) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeWaitLocked(wait)
}

func (s *Server) removeWaitLocked(wait *waiter) {
	if s.waits[wait.dev] != wait {
		return
	}
	delete(s.waits, wait.dev)
	_ = wait.conn.Close()
	if wait.dial != nil {
		_ = wait.dial.Close()
	}
	_ = wait.reader.Close()
	_ = wait.writer.Close()
}

func (s *Server) wait(wait *waiter) {
	defer s.removeWait(wait)
	// Only this handler reads the wait socket. Before pairing, waiters must be
	// silent; rejecting eager data avoids an unbounded pre-pair buffer.
	buf := make([]byte, 32*1024)
	for {
		n, err := wait.conn.Read(buf)
		if n > 0 {
			s.mu.Lock()
			paired := wait.dial != nil
			s.mu.Unlock()
			if !paired {
				return
			}
			if _, writeErr := wait.writer.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *Server) dial(conn *connection, wait *waiter, timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		// Only the first dial byte is timed. Clear the deadline before
		// forwarding it so wait-side backpressure cannot expire the relay.
		var first [1]byte
		if err := conn.SetReadDeadline(time.Now().Add(timeout)); err == nil {
			if _, err := io.ReadFull(conn, first[:]); err == nil && conn.SetReadDeadline(time.Time{}) == nil {
				if err := writeAll(wait.conn, first[:]); err == nil {
					_, _ = io.Copy(wait.conn, conn)
				}
			}
		}
		s.removeWait(wait)
		close(done)
	}()
	_, _ = io.Copy(conn, wait.reader)
	s.removeWait(wait)
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
