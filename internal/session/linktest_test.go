package session

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

func linkPair(t *testing.T, allowClient, allowOperator bool) (*Session, *Session, transport.Session, transport.Session, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	onTunnel := func(ctx context.Context, _ proto.OpenTunnel, st transport.Stream) {
		calls.Add(1)
		_, _ = io.Copy(st, st)
	}
	c, o, a, b := runPair(t,
		Options{Role: RoleClient, Initiator: true, AllowLinkTest: allowClient, Capabilities: []string{linktest.Capability}, OnTunnel: onTunnel},
		Options{Role: RoleOperator, AllowLinkTest: allowOperator, Trusted: func(identity.Fingerprint) bool { return true }, Capabilities: []string{linktest.Capability}, OnTunnel: onTunnel},
	)
	return c, o, a, b, &calls
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !condition() {
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatal("condition not reached")
		}
	}
}

func normalEcho(t *testing.T, s *Session) {
	t.Helper()
	kind := "tcp"
	if s.opts.Role == RoleClient {
		kind = "reverse"
	}
	st, err := s.OpenTunnel(testContext(t), proto.OpenTunnel{Kind: kind, Target: "127.0.0.1:6801"})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stop := context.AfterFunc(testContext(t), func() { _ = st.Close() })
	defer stop()
	if _, err := st.Write([]byte("ordinary tunnel")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("ordinary tunnel"))
	if _, err := io.ReadFull(st, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "ordinary tunnel" {
		t.Fatal(string(got))
	}
}

func TestLinkNegotiationAndPeerOnlyStreams(t *testing.T) {
	c, o, _, _, calls := linkPair(t, true, true)
	for _, s := range []*Session{c, o} {
		if len(s.NegotiatedCapabilities) != 1 || s.NegotiatedCapabilities[0] != linktest.Capability {
			t.Fatal(s.NegotiatedCapabilities)
		}
		result, err := s.TestLink(testContext(t), linktest.Options{})
		if err != nil {
			t.Fatal(err)
		}
		if result.Samples != 10 || result.BytesSent != 1<<20 || result.BytesReceived != 1<<20 || result.EchoBytesPerSecond <= 0 {
			t.Fatal(result)
		}
		if _, err := s.OpenTunnel(testContext(t), proto.OpenTunnel{Kind: linktest.Capability}); err == nil {
			t.Fatal("generic API bypassed link-test admission")
		}
	}
	if calls.Load() != 0 {
		t.Fatal("test traffic reached vehicle callbacks")
	}
	normalEcho(t, o)
	normalEcho(t, c)
	if calls.Load() != 2 {
		t.Fatal(calls.Load())
	}
}

func TestLinkUnsupportedAndUnsolicitedStreamIsolation(t *testing.T) {
	for _, allow := range [][2]bool{{false, false}, {true, false}, {false, true}} {
		c, o, a, _, calls := linkPair(t, allow[0], allow[1])
		for _, s := range []*Session{c, o} {
			if len(s.NegotiatedCapabilities) != 0 {
				t.Fatal("manual capability bypassed opt-in")
			}
			// Mutating the legacy exported slice must not grant a permission.
			s.NegotiatedCapabilities = []string{linktest.Capability}
			if _, err := s.TestLink(testContext(t), linktest.Options{}); !errors.Is(err, linktest.ErrUnsupported) {
				t.Fatal(err)
			}
		}
		st, err := a.OpenStream(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(st, proto.OpenTunnel{Kind: linktest.Capability}); err != nil {
			t.Fatal(err)
		}
		var p [1]byte
		if _, err := st.Read(p[:]); err == nil {
			t.Fatal("unsolicited stream accepted")
		}
		_ = st.Close()
		if calls.Load() != 0 {
			t.Fatal("unsolicited test reached OnTunnel")
		}
		normalEcho(t, o)
	}
}

func TestLinkInboundLimitAndIndependentOutbound(t *testing.T) {
	c, o, a, _, calls := linkPair(t, true, true)
	st, err := a.OpenStream(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := proto.Write(st, proto.OpenTunnel{Kind: linktest.Capability}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return o.linkTestIn.Load() })
	// The remote slot is busy; its rejection is confined to the new stream.
	if _, err := c.TestLink(testContext(t), linktest.Options{}); !errors.Is(err, linktest.ErrTransport) {
		t.Fatal(err)
	}
	if r, err := o.TestLink(testContext(t), linktest.Options{Samples: 1, PayloadBytes: 32}); err != nil || r.Samples != 1 {
		t.Fatalf("%+v %v", r, err)
	}
	if calls.Load() != 0 {
		t.Fatal("test reached OnTunnel")
	}
	normalEcho(t, c)
	normalEcho(t, o)
	_ = st.Close()
	waitFor(t, func() bool { return !o.linkTestIn.Load() })
	if _, err := c.TestLink(testContext(t), linktest.Options{Samples: 1, PayloadBytes: 32}); err != nil {
		t.Fatal(err)
	}
}

func TestLinkOutboundLimitCancellationAndHeaderBudget(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		ctx := testContext(t)
		a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
		wrapper := &blockedOpen{Session: a, peer: make(chan net.Conn, 1)}
		errs := make(chan error, 1)
		go func() {
			_, err := Run(ctx, b, Options{Role: RoleOperator, AllowLinkTest: true, Trusted: func(identity.Fingerprint) bool { return true }})
			errs <- err
		}()
		c, err := Run(ctx, wrapper, Options{Role: RoleClient, Initiator: true, AllowLinkTest: true})
		if err != nil {
			t.Fatal(err)
		}
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
		wrapper.block.Store(true)
		opCtx, cancel := context.WithCancel(ctx)
		type outcome struct {
			result linktest.Result
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			r, err := c.TestLink(opCtx, linktest.Options{Timeout: 100 * time.Millisecond})
			done <- outcome{r, err}
		}()
		peer := <-wrapper.peer
		if _, err := c.TestLink(ctx, linktest.Options{}); !errors.Is(err, linktest.ErrBusy) {
			t.Fatal(err)
		}
		if !timeout {
			cancel()
		}
		select {
		case got := <-done:
			want := context.Canceled
			if timeout {
				want = context.DeadlineExceeded
			}
			if !errors.Is(got.err, want) || got.result.TimedOut != timeout || c.linkTestOut.Load() {
				t.Fatalf("%+v %v", got.result, got.err)
			}
		case <-ctx.Done():
			t.Fatal("test open leaked")
		}
		cancel()
		_ = peer.Close()
		wrapper.block.Store(false)
		if err := c.Send(proto.Log{Msg: "test cancellation isolated"}); err != nil {
			t.Fatal(err)
		}
		if _, err := c.TestLink(ctx, linktest.Options{Samples: 1, PayloadBytes: 32}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLinkMalformedAndServerTimeoutIsolation(t *testing.T) {
	c, o, a, _, calls := linkPair(t, true, true)
	for _, payload := range [][]byte{
		{0, 1, 0, 1},
		{0, 0, 0, 4},
		linkRequest(1, 32, 32, 50),
	} {
		st, err := a.OpenStream(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(st, proto.OpenTunnel{Kind: linktest.Capability}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Write(payload); err != nil {
			t.Fatal(err)
		}
		// For a valid request, consume Ready and then leave the server idle.
		if len(payload) > 4 {
			ready := make([]byte, 9)
			if _, err := io.ReadFull(st, ready); err != nil {
				t.Fatal(err)
			}
		}
		var p [1]byte
		if _, err := st.Read(p[:]); err == nil {
			t.Fatal("invalid or idle stream stayed open")
		}
		_ = st.Close()
		waitFor(t, func() bool { return !o.linkTestIn.Load() })
		normalEcho(t, o)
	}
	if calls.Load() != 3 {
		t.Fatal("test frames reached vehicle callback")
	}
	if _, err := c.TestLink(testContext(t), linktest.Options{Samples: 1, PayloadBytes: 32}); err != nil {
		t.Fatal(err)
	}
}

func linkRequest(samples uint32, total uint64, frameBytes, millis uint32) []byte {
	p := make([]byte, 29)
	binary.BigEndian.PutUint32(p, 25)
	p[4] = 1
	binary.BigEndian.PutUint32(p[9:], samples)
	binary.BigEndian.PutUint64(p[13:], total)
	binary.BigEndian.PutUint32(p[21:], frameBytes)
	binary.BigEndian.PutUint32(p[25:], millis)
	return p
}

func TestLinkRejectsVehicleAddressHeader(t *testing.T) {
	_, o, a, _, calls := linkPair(t, true, true)
	for _, hdr := range []proto.OpenTunnel{
		{Kind: linktest.Capability, Target: "127.0.0.1:6801"},
		{Kind: linktest.Capability, Origin: "vehicle"},
	} {
		st, err := a.OpenStream(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(st, hdr); err != nil {
			t.Fatal(err)
		}
		var p [1]byte
		if _, err := st.Read(p[:]); err == nil {
			t.Fatal("addressed test header accepted")
		}
		_ = st.Close()
	}
	if calls.Load() != 0 || o.linkTestIn.Load() {
		t.Fatal("addressed test escaped isolation")
	}
}

func TestLinkSessionCloseReclaimsActiveTest(t *testing.T) {
	c, o, a, _, _ := linkPair(t, true, true)
	st, err := a.OpenStream(testContext(t))
	if err != nil {
		t.Fatal(err)
	}
	if err := proto.Write(st, proto.OpenTunnel{Kind: linktest.Capability}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return o.linkTestIn.Load() })
	_ = a.Close(errors.New("test disconnect"))
	for _, s := range []*Session{c, o} {
		select {
		case <-s.Done():
		case <-time.After(time.Second):
			t.Fatal("session did not close")
		}
	}
	waitFor(t, func() bool { return !o.linkTestIn.Load() })
	_ = st.Close()
}

// Only data streams are wrapped after the real memtransport handshake. This
// injects delayed reads or a blackhole without replacing session dispatch.
type impairedTransport struct {
	transport.Session
	enabled atomic.Bool
	delay   time.Duration
	reading chan struct{}
}

func (s *impairedTransport) OpenStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.OpenStream(ctx)
	if err != nil || !s.enabled.Load() {
		return st, err
	}
	return &impairedStream{Stream: st, delay: s.delay, reading: s.reading, closed: make(chan struct{})}, nil
}

type impairedStream struct {
	transport.Stream
	delay   time.Duration
	reading chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (s *impairedStream) Read(p []byte) (int, error) {
	select {
	case s.reading <- struct{}{}:
	default:
	}
	if s.delay == 0 {
		<-s.closed
		return 0, net.ErrClosed
	}
	timer := time.NewTimer(s.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return s.Stream.Read(p)
	case <-s.closed:
		return 0, net.ErrClosed
	}
}
func (s *impairedStream) Close() error {
	s.once.Do(func() { close(s.closed) })
	return s.Stream.Close()
}

func impairedPair(t *testing.T, delay time.Duration) (*Session, *Session, *impairedTransport) {
	t.Helper()
	ctx := testContext(t)
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	wrapper := &impairedTransport{Session: a, delay: delay, reading: make(chan struct{}, 1)}
	type outcome struct {
		session *Session
		err     error
	}
	done := make(chan outcome, 1)
	go func() {
		s, err := Run(ctx, b, Options{Role: RoleOperator, AllowLinkTest: true, Trusted: func(identity.Fingerprint) bool { return true }, OnTunnel: func(_ context.Context, _ proto.OpenTunnel, st transport.Stream) { _, _ = io.Copy(st, st) }})
		done <- outcome{s, err}
	}()
	c, err := Run(ctx, wrapper, Options{Role: RoleClient, Initiator: true, AllowLinkTest: true})
	if err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil {
		t.Fatal(got.err)
	}
	wrapper.enabled.Store(true)
	return c, got.session, wrapper
}

func TestLinkActiveBlackholeCancellationAndDisconnect(t *testing.T) {
	for _, mode := range []string{"cancel", "timeout", "disconnect"} {
		t.Run(mode, func(t *testing.T) {
			c, o, wrapper := impairedPair(t, 0)
			ctx, cancel := context.WithCancel(testContext(t))
			defer cancel()
			type outcome struct {
				result linktest.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				r, err := c.TestLink(ctx, linktest.Options{Timeout: 200 * time.Millisecond})
				done <- outcome{r, err}
			}()
			select {
			case <-wrapper.reading:
			case <-ctx.Done():
				t.Fatal("test did not reach echo read")
			}
			waitFor(t, func() bool { return o.linkTestIn.Load() })
			if _, err := c.TestLink(ctx, linktest.Options{}); !errors.Is(err, linktest.ErrBusy) {
				t.Fatal(err)
			}
			if mode == "cancel" {
				cancel()
			}
			if mode == "disconnect" {
				_ = wrapper.Close(errors.New("test disconnected"))
			}
			select {
			case got := <-done:
				want := context.Canceled
				if mode == "timeout" {
					want = context.DeadlineExceeded
				}
				if !errors.Is(got.err, want) || got.result.TimedOut != (mode == "timeout") || c.linkTestOut.Load() {
					t.Fatalf("%+v %v", got.result, got.err)
				}
			case <-time.After(time.Second):
				t.Fatal("active test leaked")
			}
			waitFor(t, func() bool { return !o.linkTestIn.Load() })
			if mode != "disconnect" {
				wrapper.enabled.Store(false)
				normalEcho(t, c)
				if _, err := c.TestLink(testContext(t), linktest.Options{Samples: 1, PayloadBytes: 32}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestLinkDelayedMemtransport(t *testing.T) {
	c, _, _ := impairedPair(t, 5*time.Millisecond)
	r, err := c.TestLink(testContext(t), linktest.Options{Samples: 3, PayloadBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if r.Samples != 3 || r.RTTP50 < 5*time.Millisecond || r.EchoedBytes != 1024-96 || r.EchoBytesPerSecond <= 0 {
		t.Fatal(r)
	}
}
