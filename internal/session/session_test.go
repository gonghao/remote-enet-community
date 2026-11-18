package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func runPair(t *testing.T, c, o Options) (*Session, *Session, transport.Session, transport.Session) {
	t.Helper()
	ctx := testContext(t)
	var a, b transport.Session
	if c.Initiator {
		a, b = memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	} else {
		b, a = memtransport.Pair(t, identity.Fingerprint{2}, identity.Fingerprint{1})
	}
	type result struct {
		s   *Session
		err error
	}
	ch := make(chan result, 1)
	go func() { s, err := Run(ctx, b, o); ch <- result{s, err} }()
	cs, err := Run(ctx, a, c)
	if err != nil {
		t.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatal(r.err)
	}
	return cs, r.s, a, b
}
func TestPairingAndDirection(t *testing.T) {
	for _, initiator := range []bool{true, false} {
		t.Run(fmt.Sprint(initiator), func(t *testing.T) {
			token := [16]byte{7}
			var peer identity.Fingerprint
			c, o, _, _ := runPair(t, Options{Role: RoleClient, Initiator: initiator, Token: token, Capabilities: []string{"tcp", "other"}}, Options{Role: RoleOperator, Initiator: !initiator, Capabilities: []string{"tcp"}, AcceptToken: func(p identity.Fingerprint, v [16]byte) bool { peer = p; return v == token }})
			if peer != (identity.Fingerprint{1}) || c.Peer() != (identity.Fingerprint{2}) || o.Peer() != peer {
				t.Fatal("incorrect peer")
			}
			if len(c.NegotiatedCapabilities) != 1 || c.NegotiatedCapabilities[0] != "tcp" {
				t.Fatal(c.NegotiatedCapabilities)
			}
		})
	}
}
func TestRejectedToken(t *testing.T) {
	ctx := testContext(t)
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	ch := make(chan error, 1)
	go func() {
		_, err := Run(ctx, b, Options{Role: RoleOperator, AcceptToken: func(identity.Fingerprint, [16]byte) bool { return false }})
		ch <- err
	}()
	_, err := Run(ctx, a, Options{Role: RoleClient, Initiator: true, Token: [16]byte{1}})
	if !errors.Is(err, ErrPairing) {
		t.Fatalf("client: %v", err)
	}
	if err := <-ch; !errors.Is(err, ErrPairing) {
		t.Fatalf("operator: %v", err)
	}
}
func TestVersionMismatch(t *testing.T) {
	for _, initiator := range []bool{true, false} {
		t.Run(fmt.Sprint(initiator), func(t *testing.T) {
			ctx := testContext(t)
			a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
			ch := make(chan error, 1)
			go func() {
				var st transport.Stream
				var err error
				if initiator {
					st, err = b.AcceptStream(ctx)
				} else {
					st, err = b.OpenStream(ctx)
				}
				if err != nil {
					ch <- err
					return
				}
				h := proto.Hello{ProtoVersion: 99, Role: "operator"}
				if initiator {
					_, err = proto.Read(st, proto.MaxFrameLen)
					if err == nil {
						err = proto.Write(st, h)
					}
				} else {
					err = proto.Write(st, h)
					if err == nil {
						_, err = proto.Read(st, proto.MaxFrameLen)
					}
				}
				ch <- err
			}()
			_, err := Run(ctx, a, Options{Role: RoleClient, Initiator: initiator})
			if !errors.Is(err, ErrVersion) {
				t.Fatal(err)
			}
			// Run can close the transport after decoding the incompatible Hello,
			// before the scripted peer's write or follow-up read finishes.
			if err := <-ch; err != nil && !errors.Is(err, yamux.ErrSessionShutdown) && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
				t.Fatal(err)
			}
		})
	}
}
func TestPingMessagesAndDone(t *testing.T) {
	messages := make(chan any, 1)
	c, o, a, b := runPair(t, Options{Role: RoleClient, Initiator: true, PingInterval: 5 * time.Millisecond}, Options{Role: RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }, OnMessage: func(m any) { messages <- m }, PingInterval: 5 * time.Millisecond})
	if err := c.Send(proto.VehicleState{VIN: "test"}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-messages:
		if m.(proto.VehicleState).VIN != "test" {
			t.Fatal(m)
		}
	case <-time.After(time.Second):
		t.Fatal("message timeout")
	}
	deadline := time.Now().Add(time.Second)
	for c.RTT() == 0 || o.RTT() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no RTT")
		}
		time.Sleep(time.Millisecond)
	}
	reason := errors.New("link lost")
	if err := a.Close(reason); err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Session{c, o} {
		select {
		case <-s.Done():
			if s.Err() == nil {
				t.Fatal("no error")
			}
		case <-time.After(time.Second):
			t.Fatal("Done timeout")
		}
	}
	if !errors.Is(c.Err(), reason) {
		t.Fatal(c.Err())
	}
	<-b.Done()
}
func TestConcurrentTunnels(t *testing.T) {
	handler := func(ctx context.Context, h proto.OpenTunnel, st transport.Stream) {
		data, err := io.ReadAll(st)
		if err != nil {
			return
		}
		_, _ = st.Write(append([]byte(h.Origin+":"), data...))
		_ = st.CloseWrite()
	}
	c, o, _, _ := runPair(t, Options{Role: RoleClient, Initiator: true, OnTunnel: handler}, Options{Role: RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }, OnTunnel: handler})
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		for _, s := range []*Session{c, o} {
			wg.Add(1)
			go func(s *Session) {
				defer wg.Done()
				kind := "tcp"
				if s == c {
					kind = "reverse"
				}
				st, err := s.OpenTunnel(testContext(t), proto.OpenTunnel{Kind: kind, Target: "127.0.0.1:80", Origin: "test"})
				if err != nil {
					t.Error(err)
					return
				}
				defer st.Close()
				if _, err = st.Write([]byte("request")); err != nil {
					t.Error(err)
					return
				}
				if err = st.CloseWrite(); err != nil {
					t.Error(err)
					return
				}
				got, err := io.ReadAll(st)
				if err != nil || string(got) != "test:request" {
					t.Errorf("half-close response: %q %v", got, err)
				}
			}(s)
		}
	}
	wg.Wait()
}
func TestHandshakeCancellationAndTimeout(t *testing.T) {
	t.Parallel()
	for _, duration := range []time.Duration{20 * time.Millisecond, 11 * time.Second} {
		t.Run(duration.String(), func(t *testing.T) {
			a, _ := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
			ctx, cancel := context.WithTimeout(context.Background(), duration)
			defer cancel()
			start := time.Now()
			_, err := Run(ctx, a, Options{Role: RoleOperator})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal(err)
			}
			if time.Since(start) > duration+time.Second {
				t.Fatal("late timeout")
			}
			<-a.Done()
		})
	}
}

func TestUnknownControlIsIgnored(t *testing.T) {
	messages := make(chan any, 1)
	c, _, _, _ := runPair(t, Options{Role: RoleClient, Initiator: true}, Options{Role: RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }, OnMessage: func(m any) { messages <- m }})
	if c.opts.PingInterval != time.Second {
		t.Fatal("default ping interval")
	}
	// Envelope {1:999, 2:{}} is a future message followed by a known frame.
	c.writeMu.Lock()
	_, err := c.control.Write([]byte{0, 0, 0, 7, 0xa2, 1, 0x19, 3, 0xe7, 2, 0xa0})
	c.writeMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(proto.Log{Msg: "after unknown"}); err != nil {
		t.Fatal(err)
	}
	select {
	case m := <-messages:
		if m.(proto.Log).Msg != "after unknown" {
			t.Fatal(m)
		}
	case <-time.After(time.Second):
		t.Fatal("unknown message blocked dispatch")
	}
}
func TestContextCancelsEstablishedSession(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a, b := memtransport.Pair(t, identity.Fingerprint{}, identity.Fingerprint{})
	ch := make(chan *Session, 1)
	errs := make(chan error, 1)
	go func() {
		s, err := Run(ctx, b, Options{Role: RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }})
		if err != nil {
			errs <- err
			return
		}
		ch <- s
	}()
	c, err := Run(ctx, a, Options{Role: RoleClient, Initiator: true})
	if err != nil {
		t.Fatal(err)
	}
	var o *Session
	select {
	case o = <-ch:
	case err := <-errs:
		t.Fatal(err)
	}
	cancel()
	for _, s := range []*Session{c, o} {
		select {
		case <-s.Done():
		case <-time.After(time.Second):
			t.Fatal("cancellation not propagated")
		}
	}
	if err := c.Send(proto.Ping{}); err == nil {
		t.Fatal("Send after cancel")
	}
	if _, err := c.OpenTunnel(context.Background(), proto.OpenTunnel{Kind: "reverse", Target: "127.0.0.1:80"}); err == nil {
		t.Fatal("OpenTunnel after cancel")
	}
}
