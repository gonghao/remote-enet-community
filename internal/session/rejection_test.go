package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

// Hold the yamux send worker after the peer has received the payload but
// before the worker can report success to the stream writer.
type delayedWriteConn struct {
	net.Conn
	prefix    []byte
	delivered chan struct{}
	resume    chan struct{}
	ctx       context.Context
	cancel    context.CancelFunc
}

func (c *delayedWriteConn) Close() error {
	c.cancel()
	return c.Conn.Close()
}

func (c *delayedWriteConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil && bytes.HasPrefix(p, c.prefix) {
		close(c.delivered)
		select {
		case <-c.resume:
		case <-c.ctx.Done():
			return n, c.ctx.Err()
		}
	}
	return n, err
}

// Resume the client's outstanding write when the operator has sent its FIN.
type rejectionObserver struct {
	transport.Session
	fin chan struct{}
}

func (o rejectionObserver) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := o.Session.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	return &observedFIN{st, o.fin}, nil
}

type observedFIN struct {
	transport.Stream
	fin chan struct{}
}

func (s *observedFIN) CloseWrite() error {
	err := s.Stream.CloseWrite()
	close(s.fin)
	return err
}

func TestRejectionBeforeClientWriteReturns(t *testing.T) {
	for _, tc := range []struct {
		name    string
		request any
		want    error
	}{
		{"commit", proto.PakeCommit{}, cpace.ReasonUnavailable},
		{"confirmation", proto.PakeConfirmA{}, cpace.ReasonStoreFailed},
		{"token", proto.PairToken{}, ErrPairing},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			left, right := net.Pipe()
			// Match the canonical CBOR envelope through the message number.
			body := encodedMessages(t, tc.request)[4:]
			writeCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			delay := &delayedWriteConn{
				Conn: left, prefix: body[:3], ctx: writeCtx, cancel: cancel,
				delivered: make(chan struct{}), resume: make(chan struct{}),
			}
			cfp, ofp := identity.Fingerprint{1}, identity.Fingerprint{2}
			a, b := memtransport.PairConns(t, delay, right, cfp, ofp)
			client := Options{Role: RoleClient, Initiator: true, LocalFP: cfp, DigitalCode: "00001234"}
			operator := Options{Role: RoleOperator, LocalFP: ofp}
			switch tc.request.(type) {
			case proto.PakeCommit:
				operator.DigitalManager = testResponder{refusal: cpace.ReasonUnavailable}
			case proto.PakeConfirmA:
				operator.DigitalManager = testResponder{code: client.DigitalCode, commitErr: cpace.ReasonStoreFailed}
			case proto.PairToken:
				client.DigitalCode = ""
				client.Token = [16]byte{1}
			}
			exporter := [32]byte{3}
			opResult := make(chan error, 1)
			go func() {
				_, err := Run(ctx, exportSession{rejectionObserver{b, delay.resume}, exporter}, operator)
				opResult <- err
			}()
			_, err := Run(ctx, exportSession{a, exporter}, client)
			if !errors.Is(err, tc.want) {
				t.Errorf("client: got %v, want %v", err, tc.want)
			}
			select {
			case <-delay.delivered:
			default:
				t.Error("request was not delivered")
			}
			if err := <-opResult; !errors.Is(err, tc.want) {
				t.Errorf("operator: got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRejectionOverTCPAndTLS(t *testing.T) {
	ctx := testContext(t)
	c, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	o, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp4", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, err := listener.Accept()
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	run := func(conn net.Conn, opts secure.Opts, sessionOpts Options) error {
		ts, err := secure.WrapStream(ctx, conn, opts)
		if err != nil {
			return err
		}
		defer ts.Close(nil)
		_, err = Run(ctx, ts, sessionOpts)
		return err
	}
	res := make(chan error, 1)
	go func() {
		res <- run(peer, secure.Opts{Local: o, Expect: c.Fingerprint()}, Options{
			Role: RoleOperator, LocalFP: o.Fingerprint(), DigitalManager: testResponder{code: "00001234"},
			Remember: func(context.Context, identity.Fingerprint) error { return errors.New("disk full") },
		})
	}()
	err = run(conn, secure.Opts{Local: c, Expect: o.Fingerprint(), IsClient: true}, Options{
		Role: RoleClient, Initiator: true, LocalFP: c.Fingerprint(), DigitalCode: "00001234",
	})
	if !errors.Is(err, cpace.ReasonStoreFailed) {
		t.Errorf("client: %v", err)
	}
	if err := <-res; !errors.Is(err, cpace.ReasonStoreFailed) {
		t.Errorf("operator: %v", err)
	}
}

// A peer that received the rejection but keeps its side of the control stream open.
type stalledRejectionPeer struct {
	*handshakeTransport
	draining chan struct{}
}

func (p stalledRejectionPeer) AcceptStream(context.Context) (transport.Stream, error) {
	return &prefixedStream{p.stream, io.MultiReader(p.stream.input, p)}, nil
}

func (p stalledRejectionPeer) Read([]byte) (int, error) {
	close(p.draining)
	<-p.Done()
	return 0, io.EOF
}

func TestRejectionDrainStops(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		name := "timeout"
		if cancelCaller {
			name = "cancel"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
				hello := proto.Hello{ProtoVersion: proto.Version, Role: "client", Capabilities: []string{proto.PairDirectCapability}}
				ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t, hello, proto.PakeCommit{}))}
				peer := stalledRejectionPeer{ts, make(chan struct{})}
				result := make(chan error, 1)
				go func() {
					_, err := Run(ctx, exportSession{peer, [32]byte{3}}, Options{Role: RoleOperator, LocalFP: identity.Fingerprint{2}, DigitalManager: testResponder{refusal: cpace.ReasonUnavailable}})
					result <- err
				}()
				<-peer.draining
				start := time.Now()
				if cancelCaller {
					cancel()
				}
				if err := <-result; !errors.Is(err, cpace.ReasonUnavailable) {
					t.Fatal(err)
				}
				want := handshakeTimeout
				if cancelCaller {
					want = 0
				}
				if elapsed := time.Since(start); elapsed != want {
					t.Fatalf("drain took %s, want %s", elapsed, want)
				}
			})
		})
	}
}
