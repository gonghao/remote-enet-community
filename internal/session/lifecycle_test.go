package session

import (
	"context"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

func TestTrustedPeerMaySendToken(t *testing.T) {
	for _, initiator := range []bool{true, false} {
		t.Run(map[bool]string{true: "client-dials", false: "operator-dials"}[initiator], func(t *testing.T) {
			delivered := make(chan any, 1)
			c, _, _, _ := runPair(t, Options{Role: RoleClient, Initiator: initiator, Token: [16]byte{9}}, Options{Role: RoleOperator, Initiator: !initiator, Trusted: func(identity.Fingerprint) bool { return true }, AcceptToken: func(identity.Fingerprint, [16]byte) bool {
				t.Error("trusted peer must bypass token validation")
				return false
			}, OnMessage: func(m any) { delivered <- m }})
			if err := c.Send(proto.Log{Msg: "after token"}); err != nil {
				t.Fatal(err)
			}
			select {
			case msg := <-delivered:
				if msg.(proto.Log).Msg != "after token" {
					t.Fatal(msg)
				}
			case <-time.After(time.Second):
				t.Fatal("token disrupted control stream")
			}
		})
	}
}
func TestUntrustedWithoutTokenTimesOut(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	errs := make(chan error, 2)
	start := time.Now()
	go func() { _, err := Run(ctx, a, Options{Role: RoleClient, Initiator: true}); errs <- err }()
	go func() { _, err := Run(ctx, b, Options{Role: RoleOperator}); errs <- err }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("untrusted peer accepted")
			}
		case <-ctx.Done():
			t.Fatal("handshake did not terminate")
		}
	}
	elapsed := time.Since(start)
	if elapsed < handshakeTimeout-100*time.Millisecond || elapsed > handshakeTimeout+time.Second {
		t.Fatalf("unexpected handshake duration %v", elapsed)
	}
	for _, ts := range []transport.Session{a, b} {
		select {
		case <-ts.Done():
		case <-ctx.Done():
			t.Fatal("transport leaked")
		}
	}
}
func TestDispatchTimeoutAndCancellation(t *testing.T) {
	t.Parallel()
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "header-timeout"}[timeout], func(t *testing.T) {
			a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := a.OpenStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			st, err := b.AcceptStream(ctx)
			if err != nil {
				t.Fatal(err)
			}
			s := &Session{ctx: ctx, opts: Options{Role: RoleClient}}
			done := make(chan struct{})
			go func() { s.dispatch(st); close(done) }()
			limit := time.Second
			if timeout {
				limit = handshakeTimeout + time.Second
			} else {
				cancel()
			}
			select {
			case <-done:
			case <-time.After(limit):
				t.Fatal("header reader leaked")
			}
			// Timing out an individual stream must leave the transport intact.
			select {
			case <-b.Done():
				t.Fatal("header timeout closed transport")
			default:
			}
		})
	}
}

type blockedOpen struct {
	transport.Session
	block atomic.Bool
	peer  chan net.Conn
}

func (t *blockedOpen) OpenStream(ctx context.Context) (transport.Stream, error) {
	if !t.block.Load() {
		return t.Session.OpenStream(ctx)
	}
	a, b := net.Pipe()
	t.peer <- b
	return &blockedStream{a}, nil
}

type blockedStream struct{ net.Conn }

func (s *blockedStream) CloseWrite() error { return s.Close() }

func TestOpenTunnelCancellationIsLocal(t *testing.T) {
	ctx := testContext(t)
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	wrapper := &blockedOpen{Session: a, peer: make(chan net.Conn, 1)}
	errs := make(chan error, 1)
	go func() {
		_, err := Run(ctx, b, Options{Role: RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }})
		errs <- err
	}()
	c, err := Run(ctx, wrapper, Options{Role: RoleClient, Initiator: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-errs; err != nil {
		t.Fatal(err)
	}
	wrapper.block.Store(true)
	opCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() {
		_, err := c.OpenTunnel(opCtx, proto.OpenTunnel{Kind: "reverse", Target: "127.0.0.1:80"})
		done <- err
	}()
	peer := <-wrapper.peer
	defer peer.Close()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("blocked header write leaked")
	}
	if err := c.Send(proto.Log{Msg: "still alive"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-a.Done():
		t.Fatal("open cancellation destroyed session")
	default:
	}
	wrapper.block.Store(false)
	// A new data stream still opens after cancellation of the previous header.
	st, err := c.OpenTunnel(ctx, proto.OpenTunnel{Kind: "reverse", Target: "127.0.0.1:80"})
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
}
func TestHandshakeReadAndWriteCancellation(t *testing.T) {
	for _, initiator := range []bool{true, false} {
		t.Run(map[bool]string{true: "write", false: "read"}[initiator], func(t *testing.T) {
			x, y := net.Pipe()
			defer y.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			ts := &singleStreamSession{conn: x, done: make(chan struct{})}
			_, err := Run(ctx, ts, Options{Role: RoleClient, Initiator: initiator})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
		})
	}
}

// This transport deliberately has no stream buffering, so handshake writes block.
type singleStreamSession struct {
	conn   net.Conn
	done   chan struct{}
	closed atomic.Bool
}

func (s *singleStreamSession) OpenStream(context.Context) (transport.Stream, error) {
	return &blockedStream{s.conn}, nil
}
func (s *singleStreamSession) AcceptStream(context.Context) (transport.Stream, error) {
	return &blockedStream{s.conn}, nil
}
func (s *singleStreamSession) Peer() identity.Fingerprint { return identity.Fingerprint{} }
func (s *singleStreamSession) Path() transport.PathInfo   { return transport.PathInfo{} }
func (s *singleStreamSession) Stats() transport.LinkStats { return transport.LinkStats{} }
func (s *singleStreamSession) Close(error) error {
	if s.closed.CompareAndSwap(false, true) {
		close(s.done)
	}
	return s.conn.Close()
}
func (s *singleStreamSession) Done() <-chan struct{} { return s.done }
func (s *singleStreamSession) Err() error {
	if s.closed.Load() {
		return io.EOF
	}
	return nil
}
