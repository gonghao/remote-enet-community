// Package secure is the end-to-end encryption boundary: mutual TLS 1.3
// authenticates device keys before yamux can interpret any application bytes.
package secure

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

// Opts selects the local identity and the expected remote public-key fingerprint.
// A zero Expect is for token-authenticated first pairing only.
type Opts struct {
	Local     *identity.Local
	Expect    identity.Fingerprint
	IsClient  bool
	Path      transport.PathInfo
	ALPN      string
	KeepAlive time.Duration
}

// WrapStream takes ownership of conn, including on failure. ctx controls both
// handshake and session lifetime. Callers should set a handshake deadline.
func WrapStream(ctx context.Context, conn net.Conn, o Opts) (transport.Session, error) {
	if o.Local == nil {
		conn.Close()
		return nil, fmt.Errorf("secure: local identity required")
	}
	if o.ALPN == "" {
		o.ALPN = "renet/1"
	}
	if len(o.ALPN) > 255 || o.KeepAlive < 0 {
		conn.Close()
		return nil, fmt.Errorf("secure: invalid options")
	}
	if o.KeepAlive == 0 {
		o.KeepAlive = 15 * time.Second
	}
	var peer identity.Fingerprint
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
		Certificates: []tls.Certificate{o.Local.Cert},
		ClientAuth:   tls.RequireAnyClientCert,
		// Device identities use self-signed keys, not the public CA/DNS PKI.
		InsecureSkipVerify: true,
		NextProtos:         []string{o.ALPN}, SessionTicketsDisabled: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) != 1 {
				return fmt.Errorf("secure: expected one self-signed certificate")
			}
			cert, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return fmt.Errorf("secure: parse certificate: %w", err)
			}
			fp, err := identity.FingerprintOfCert(cert)
			if err != nil {
				return fmt.Errorf("secure: peer key: %w", err)
			}
			if !bytes.Equal(cert.RawIssuer, cert.RawSubject) {
				return fmt.Errorf("secure: certificate is not self-issued")
			}
			if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
				return fmt.Errorf("secure: self-signature: %w", err)
			}
			if len(cert.UnhandledCriticalExtensions) != 0 {
				return fmt.Errorf("secure: unsupported critical certificate extension")
			}
			now := time.Now()
			if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return fmt.Errorf("secure: certificate outside validity period")
			}
			if !o.Expect.IsZero() && fp != o.Expect {
				return fmt.Errorf("secure: peer %w", identity.ErrFingerprintMismatch)
			}
			peer = fp
			return nil
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.NegotiatedProtocol != o.ALPN {
				return fmt.Errorf("secure: ALPN mismatch")
			}
			return nil
		},
	}
	var tc *tls.Conn
	if o.IsClient {
		tc = tls.Client(conn, cfg)
	} else {
		tc = tls.Server(conn, cfg)
	}
	if err := tc.HandshakeContext(ctx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("secure: TLS handshake: %w", err)
	}
	state := tc.ConnectionState()
	exporter, err := state.ExportKeyingMaterial("EXPORTER-renet-pake-sid-v1", nil, 32)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("secure: pairing exporter: %w", err)
	}
	// TLS 1.3 lets the client finish before the server verifies its certificate.
	// Wait for the authenticated server's acceptance before starting yamux, so
	// a rejected client never emits application data. This single byte is part
	// of the renet/1 wire protocol, inside TLS and before yamux.
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	var ready [1]byte
	ready[0] = 1
	var confirmErr error
	if o.IsClient {
		_, confirmErr = io.ReadFull(tc, ready[:])
		if confirmErr == nil && ready[0] != 1 {
			confirmErr = fmt.Errorf("invalid acceptance byte")
		}
	} else {
		_, confirmErr = tc.Write(ready[:])
	}
	stop()
	if confirmErr != nil {
		conn.Close()
		return nil, fmt.Errorf("secure: TLS acceptance: %w", confirmErr)
	}
	if err := ctx.Err(); err != nil {
		conn.Close()
		return nil, err
	}
	yc := yamux.DefaultConfig()
	// Our periodic Ping measures RTT as well as detecting an unresponsive peer.
	yc.EnableKeepAlive = false
	yc.KeepAliveInterval = o.KeepAlive
	yc.LogOutput = nil
	yc.Logger = muxLogger{}
	var mux *yamux.Session
	if o.IsClient {
		mux, err = yamux.Client(tc, yc)
	} else {
		mux, err = yamux.Server(tc, yc)
	}
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("secure: yamux: %w", err)
	}
	s := &session{exporter: [32]byte(exporter), mux: mux, raw: conn, peer: peer, path: o.Path, done: make(chan struct{}), openGate: make(chan struct{}, 1)}
	go func() {
		select {
		case <-ctx.Done():
			s.Close(ctx.Err())
		case <-mux.CloseChan():
			s.closeOnError(ctx, io.EOF)
		case <-s.done:
		}
	}()
	go s.ping(ctx, o.KeepAlive)
	return s, nil
}

type muxLogger struct{}

func (muxLogger) Print(args ...interface{})   { slog.Debug("yamux", "message", fmt.Sprint(args...)) }
func (muxLogger) Println(args ...interface{}) { slog.Debug("yamux", "message", fmt.Sprintln(args...)) }

func (muxLogger) Printf(format string, args ...interface{}) {
	slog.Debug("yamux", "message", fmt.Sprintf(format, args...))
}

type session struct {
	exporter [32]byte
	mux      *yamux.Session
	raw      net.Conn
	peer     identity.Fingerprint
	path     transport.PathInfo
	done     chan struct{}
	once     sync.Once
	mu       sync.RWMutex
	err      error
	rtt      atomic.Int64
	openGate chan struct{}
}

type stream struct {
	*yamux.Stream
	closed atomic.Bool
}

func (s *stream) Read(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	return s.Stream.Read(p)
}

func (s *stream) Write(p []byte) (int, error) {
	if s.closed.Load() {
		return 0, net.ErrClosed
	}
	return s.Stream.Write(p)
}

// Expire this stream's I/O before sending FIN: yamux Close alone preserves
// reads and cannot release a copy waiting for a peer that never sends FIN.
func (s *stream) Close() error {
	if s.closed.Swap(true) {
		return nil
	}
	s.Stream.SetDeadline(time.Now())
	return s.Stream.Close()
}

// yamux Close sends FIN while preserving reads until the peer also sends FIN.
func (s *stream) CloseWrite() error { return s.Stream.Close() }

func (s *session) OpenStream(ctx context.Context) (transport.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.openGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.Err()
	}
	type result struct {
		st  *yamux.Stream
		err error
	}
	ch := make(chan result)
	// Only one opener may block inside yamux. Cancellation returns immediately;
	// the worker disposes a late result and exits on completion or session close.
	// yamux has no cancellable OpenStream: while its SYN backlog is full, this
	// one worker retains the gate until ACK, StreamOpenTimeout, or session close.
	// Do not close the shared session on cancellation of an individual request.
	go func() {
		defer func() { <-s.openGate }()
		st, err := s.mux.OpenStream()
		select {
		case ch <- result{st, err}:
		case <-ctx.Done():
			if st != nil {
				(&stream{Stream: st}).Close()
			}
		case <-s.done:
			if st != nil {
				(&stream{Stream: st}).Close()
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			return nil, fmt.Errorf("secure: open stream: %w", r.err)
		}
		return &stream{Stream: r.st}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, s.Err()
	}
}
func (s *session) AcceptStream(ctx context.Context) (transport.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	st, err := s.mux.AcceptStreamWithContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("secure: accept stream: %w", err)
	}
	return &stream{Stream: st}, nil
}
func (s *session) PairingExporter() [32]byte { return s.exporter }

func (s *session) Peer() identity.Fingerprint { return s.peer }
func (s *session) Path() transport.PathInfo   { return s.path }
func (s *session) Stats() transport.LinkStats {
	return transport.LinkStats{RTT: time.Duration(s.rtt.Load())}
}
func (s *session) Done() <-chan struct{} { return s.done }
func (s *session) Err() error            { s.mu.RLock(); defer s.mu.RUnlock(); return s.err }
func (s *session) Close(reason error) error {
	s.once.Do(func() {
		if reason == nil {
			reason = io.EOF
		}
		s.mu.Lock()
		s.err = reason
		s.mu.Unlock()
		// Close the raw connection first so TLS close_notify cannot block teardown.
		s.raw.Close()
		s.mux.Close()
		close(s.done)
	})
	return nil
}
func (s *session) closeOnError(ctx context.Context, reason error) {
	// A peer may close first in response to the same lifetime cancellation.
	// Prefer cancellation already visible here, but leave explicit Close intact.
	if err := ctx.Err(); err != nil {
		reason = err
	}
	s.Close(reason)
}

func (s *session) ping(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			rtt, err := s.mux.Ping()
			if err != nil {
				s.closeOnError(ctx, fmt.Errorf("secure: ping: %w", err))
				return
			}
			s.rtt.Store(int64(rtt))
		}
	}
}
