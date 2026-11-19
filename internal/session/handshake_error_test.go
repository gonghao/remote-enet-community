package session

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

func encodedMessages(t *testing.T, messages ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	for _, message := range messages {
		if err := proto.Write(&buf, message); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes()
}

// A drop is observed by Run's transport watcher before the final Read/Write
// returns. This fixes the scheduling order that previously hid protocol errors.
type handshakeTransport struct {
	stream *handshakeStream
	done   chan struct{}
	closed chan struct{}
	mu     sync.Mutex
	err    error
	once   sync.Once
}

type handshakeStream struct {
	owner          *handshakeTransport
	input          *bytes.Reader
	written        int
	dropAfterRead  bool
	dropAfterWrite int
	cause          error
	afterRead      func()
}

func (s *handshakeStream) Read(p []byte) (int, error) {
	n, err := s.input.Read(p)
	if s.afterRead != nil && s.input.Len() == 0 {
		afterRead := s.afterRead
		s.afterRead = nil
		afterRead()
	}
	if s.dropAfterRead && s.input.Len() == 0 {
		s.dropAfterRead = false
		s.owner.drop(s.cause)
	}
	return n, err
}

func (s *handshakeStream) Write(p []byte) (int, error) {
	s.written += len(p)
	if s.dropAfterWrite != 0 && s.written >= s.dropAfterWrite {
		s.dropAfterWrite = 0
		s.owner.drop(s.cause)
	}
	return len(p), nil
}

func (s *handshakeStream) Close() error      { return nil }
func (s *handshakeStream) CloseWrite() error { return nil }

func (s *handshakeTransport) drop(reason error) {
	s.terminate(reason)
	<-s.closed
}

func (s *handshakeTransport) terminate(reason error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err == nil {
		s.err = reason
		close(s.done)
	}
}

func (s *handshakeTransport) OpenStream(context.Context) (transport.Stream, error) {
	return s.stream, nil
}
func (s *handshakeTransport) AcceptStream(context.Context) (transport.Stream, error) {
	return s.stream, nil
}
func (s *handshakeTransport) Peer() identity.Fingerprint { return identity.Fingerprint{1} }
func (s *handshakeTransport) Path() transport.PathInfo   { return transport.PathInfo{} }
func (s *handshakeTransport) Stats() transport.LinkStats { return transport.LinkStats{} }
func (s *handshakeTransport) Close(reason error) error {
	if reason == nil {
		reason = io.EOF
	}
	s.terminate(reason)
	s.once.Do(func() { close(s.closed) })
	return nil
}
func (s *handshakeTransport) Done() <-chan struct{} { return s.done }
func (s *handshakeTransport) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

func TestHandshakeErrorSurvivesTransportShutdown(t *testing.T) {
	for _, tc := range []struct {
		name        string
		options     Options
		incoming    []byte
		dropOnWrite int
		want        error
	}{
		{
			name:     "client-rejection",
			options:  Options{Role: RoleClient, Initiator: true, Token: [16]byte{9}},
			incoming: encodedMessages(t, proto.Hello{ProtoVersion: proto.Version, Role: string(RoleOperator)}, proto.PairResult{Accepted: false, Reason: "invalid pairing token"}),
			want:     ErrPairing,
		},
		{
			name:        "operator-rejection",
			options:     Options{Role: RoleOperator, AcceptToken: func(identity.Fingerprint, [16]byte) bool { return false }},
			incoming:    encodedMessages(t, proto.Hello{ProtoVersion: proto.Version, Role: string(RoleClient)}, proto.PairToken{Token: [16]byte{9}}),
			dropOnWrite: len(encodedMessages(t, proto.Hello{ProtoVersion: proto.Version, Role: string(RoleOperator)}, proto.PairResult{Accepted: false, Reason: "invalid pairing token"})),
			want:        ErrPairing,
		},
		{
			name:     "version-mismatch",
			options:  Options{Role: RoleClient, Initiator: true},
			incoming: encodedMessages(t, proto.Hello{ProtoVersion: 99, Role: string(RoleOperator)}),
			want:     ErrVersion,
		},
		{
			name:     "transport-failure",
			options:  Options{Role: RoleClient, Initiator: true},
			incoming: []byte{0, 0},
			want:     io.ErrUnexpectedEOF,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cause := fmt.Errorf("test transport lost")
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(tc.incoming), dropAfterRead: tc.dropOnWrite == 0, dropAfterWrite: tc.dropOnWrite, cause: cause}
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err := Run(ctx, ts, tc.options)
			if !errors.Is(err, tc.want) {
				t.Fatalf("handshake error=%v, want %v", err, tc.want)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("internal shutdown replaced the handshake result: %v", err)
			}
			if tc.want == io.ErrUnexpectedEOF && !errors.Is(err, cause) {
				t.Fatalf("transport cause was lost: %v", err)
			}
		})
	}
}

func TestLegacyPairingRejectionWhitelistsPeerReason(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   string
	}{
		{invalidPairingToken, "session: handshake: session: pairing rejected: invalid pairing token"},
		{"remote text with a fake 0482 7193 code", "session: handshake: session: pairing rejected"},
		{"expired", "session: handshake: session: pairing rejected"},
	} {
		ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
		ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t,
			proto.Hello{ProtoVersion: proto.Version, Role: string(RoleOperator)},
			proto.PairResult{Reason: tc.reason},
		))}
		_, err := Run(t.Context(), ts, Options{Role: RoleClient, Initiator: true})
		if !errors.Is(err, ErrPairing) || err.Error() != tc.want {
			t.Errorf("legacy rejection = %v, want %q", err, tc.want)
		}
	}
}
