package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

type exportSession struct {
	transport.Session
	exporter [32]byte
}

func (s exportSession) PairingExporter() [32]byte { return s.exporter }

type testResponder struct {
	code      string
	refusal   cpace.Reason
	commitErr error
}

func (r testResponder) Begin(ctx context.Context) (DigitalAttempt, error) {
	if r.refusal != "" {
		return nil, r.refusal
	}
	return testAttempt{ctx, r.code, r.commitErr}, nil
}

type testAttempt struct {
	ctx       context.Context
	code      string
	commitErr error
}

func (a testAttempt) Context() context.Context { return a.ctx }
func (a testAttempt) Finish()                  {}
func (a testAttempt) Respond(b cpace.Binding, y [32]byte) (*cpace.Responder, [32]byte, error) {
	return cpace.Respond(a.code, b, y)
}
func (a testAttempt) Commit(save func(context.Context) error) error {
	if a.commitErr != nil {
		return a.commitErr
	}
	if err := save(a.ctx); err != nil {
		return cpace.ReasonStoreFailed
	}
	return nil
}

// Give the operator an equivalent Hello with a non-minimal envelope key.
type alteredHello struct{ transport.Session }

func (s alteredHello) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	var prefix [4]byte
	if _, err = io.ReadFull(st, prefix[:]); err != nil {
		return nil, err
	}
	raw := make([]byte, binary.BigEndian.Uint32(prefix[:]))
	if _, err = io.ReadFull(st, raw); err != nil {
		return nil, err
	}
	frame := binary.BigEndian.AppendUint32(nil, uint32(len(raw)+1))
	frame = append(frame, raw[0], 0x18)
	frame = append(frame, raw[1:]...)
	return &prefixedStream{st, io.MultiReader(bytes.NewReader(frame), st)}, nil
}

type prefixedStream struct {
	transport.Stream
	reader io.Reader
}

func (s *prefixedStream) Read(p []byte) (int, error) { return s.reader.Read(p) }

func TestDigitalExchange(t *testing.T) {
	const code = "00001234"
	for _, tc := range []struct {
		name string
		want cpace.Reason
	}{
		{"success", ""}, {"wrong code", cpace.ReasonConfirmation}, {"altered hello", cpace.ReasonConfirmation}, {"exporter mismatch", cpace.ReasonConfirmation}, {"save failed", cpace.ReasonStoreFailed}, {"no code", cpace.ReasonUnavailable},
		{"commit canceled", ReasonInterrupted}, {"commit deadline", ReasonInterrupted}, {"save deadline", cpace.ReasonStoreFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := testContext(t)
			cfp, ofp := identity.Fingerprint{1}, identity.Fingerprint{2}
			a, b := memtransport.Pair(t, cfp, ofp)
			c := Options{Role: RoleClient, Initiator: true, LocalFP: cfp, DigitalCode: code}
			m := testResponder{code: code}
			exp := [32]byte{9}
			serverExp := exp
			switch tc.name {
			case "wrong code":
				m.code = "00001235"
			case "altered hello":
				b = alteredHello{b}
			case "exporter mismatch":
				serverExp[0]++
			case "no code":
				m.refusal = cpace.ReasonUnavailable
			case "commit canceled":
				m.commitErr = context.Canceled
			case "commit deadline":
				m.commitErr = context.DeadlineExceeded
			}
			saves := 0
			o := Options{Role: RoleOperator, LocalFP: ofp, DigitalManager: m, Trusted: func(identity.Fingerprint) bool { return true }, Remember: func(ctx context.Context, peer identity.Fingerprint) error {
				saves++
				if peer != cfp {
					t.Errorf("wrong peer saved")
				}
				switch tc.name {
				case "save failed":
					return errors.New(code)
				case "save deadline":
					return context.DeadlineExceeded
				}
				return nil
			}}
			type result struct {
				s   *Session
				err error
			}
			done := make(chan result, 1)
			go func() { s, err := Run(ctx, exportSession{b, serverExp}, o); done <- result{s, err} }()
			cs, err := Run(ctx, exportSession{a, exp}, c)
			op := <-done
			if tc.want == "" {
				if err != nil || op.err != nil || cs.PairingOutcome() != DigitalPaired || op.s.PairingOutcome() != DigitalPaired || cs.opts.DigitalCode != "" {
					t.Fatalf("pairing failed: %v %v", err, op.err)
				}
			} else {
				for _, failure := range []error{err, op.err} {
					if !errors.Is(failure, tc.want) || strings.Contains(failure.Error(), code) {
						t.Fatalf("unexpected error: %v", failure)
					}
				}
				if cs != nil || op.s != nil {
					t.Fatal("failed handshake returned session")
				}
			}
			wantSaves := 0
			if tc.want == "" || tc.want == cpace.ReasonStoreFailed {
				wantSaves = 1
			}
			if saves != wantSaves {
				t.Fatalf("saved %d times", saves)
			}
		})
	}
}

type recordingTransport struct {
	transport.Session
	output bytes.Buffer
}

func (s *recordingTransport) OpenStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return &recordedStream{st, io.MultiWriter(st, &s.output)}, nil
}

type recordedStream struct {
	transport.Stream
	writer io.Writer
}

func (s *recordedStream) Write(p []byte) (int, error) { return s.writer.Write(p) }

func TestDigitalClientScript(t *testing.T) {
	const code = "00001234"
	_, share, err := cpace.Start(code, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	shareB := proto.PairShareB{Value: share}
	for _, tc := range []struct {
		name             string
		replies          []any
		want             cpace.Reason
		shares, confirms int
	}{
		{"unsupported", nil, ReasonUnsupported, 0, 0},
		{"missing exporter", nil, ReasonInterrupted, 0, 0},
		{"early accepted", []any{proto.PairResult{Accepted: true}}, ReasonInterrupted, 1, 0},
		{"late accepted", []any{shareB, proto.PairResult{Accepted: true}}, ReasonInterrupted, 1, 1},
		{"lost after confirmation", []any{shareB}, ReasonInterrupted, 1, 1},
		{"wrong order", []any{proto.PairConfirmB{}}, ReasonInterrupted, 1, 0},
		{"duplicate share", []any{shareB, shareB}, ReasonInterrupted, 1, 1},
		{"wrong tag", []any{shareB, proto.PairConfirmB{}}, cpace.ReasonConfirmation, 1, 1},
		{"unknown reason", []any{proto.PairResult{Reason: code}}, ReasonInterrupted, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := proto.Hello{Role: "operator", ProtoVersion: proto.Version}
			if tc.name != "unsupported" {
				h.Capabilities = []string{proto.PairDirectCapability}
			}
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t, append([]any{h}, tc.replies...)...))}
			record := &recordingTransport{Session: ts}
			var wire transport.Session = exportSession{record, [32]byte{1}}
			if tc.name == "missing exporter" {
				wire = record
			}
			s, err := Run(testContext(t), wire, Options{Role: RoleClient, Initiator: true, LocalFP: identity.Fingerprint{2}, DigitalCode: code, Token: [16]byte{1}})
			if s != nil || !errors.Is(err, tc.want) || strings.Contains(err.Error(), code) {
				t.Fatalf("unexpected result: %v", err)
			}
			msg, err := proto.Read(&record.output, proto.MaxFrameLen)
			if err != nil || len(msg.(proto.Hello).Capabilities) != 1 {
				t.Fatal("missing pairing capability")
			}
			shares, confirms := 0, 0
			for record.output.Len() > 0 {
				msg, err := proto.ReadPair(&record.output)
				if err != nil {
					t.Fatal(err)
				}
				switch msg.(type) {
				case proto.PairShareA:
					shares++
				case proto.PairConfirmA:
					confirms++
				default:
					t.Fatalf("unexpected %T", msg)
				}
			}
			if shares != tc.shares || confirms != tc.confirms {
				t.Fatalf("sent %d shares and %d confirmations", shares, confirms)
			}
		})
	}
}

func TestDigitalRejectionSurvivesHandshakeDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hello := proto.Hello{Role: "operator", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}
		ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
		ts.stream = &handshakeStream{
			owner: ts,
			input: bytes.NewReader(encodedMessages(t, hello, proto.PairResult{Reason: string(cpace.ReasonExpired)})),
			afterRead: func() {
				time.Sleep(handshakeTimeout)
				<-ts.closed
			},
		}
		s, err := Run(t.Context(), exportSession{ts, [32]byte{1}}, Options{
			Role: RoleClient, Initiator: true, LocalFP: identity.Fingerprint{2}, DigitalCode: "00001234",
		})
		if s != nil || !errors.Is(err, ErrPairing) || !errors.Is(err, cpace.ReasonExpired) {
			t.Fatalf("deadline replaced decoded rejection: %v", err)
		}
	})
}

type managerResponder struct{ *pairing.Manager }

func (m managerResponder) Begin(ctx context.Context) (DigitalAttempt, error) {
	return m.Manager.Begin(ctx)
}

type closingScriptTransport struct {
	transport.Session
	inputSize int
	output    bytes.Buffer
	ctx       context.Context
}

func (s *closingScriptTransport) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	reader := &closingScriptReader{reader: st, remaining: s.inputSize, done: s.Done(), ctx: s.ctx}
	return &recordedStream{&prefixedStream{st, reader}, io.MultiWriter(&s.output, st)}, nil
}

type closingScriptReader struct {
	reader    io.Reader
	remaining int
	done      <-chan struct{}
	ctx       context.Context
}

func (r *closingScriptReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.remaining -= n
	if err == nil && r.remaining == 0 {
		// Deliver the complete buffered confirmation after shutdown is observable.
		select {
		case <-r.done:
		case <-r.ctx.Done():
			return n, r.ctx.Err()
		}
	}
	return n, err
}

func TestDigitalOperatorDisconnectAfterClientConfirmation(t *testing.T) {
	ctx := testContext(t)
	cfp, ofp := identity.Fingerprint{1}, identity.Fingerprint{2}
	exp := [32]byte{9}
	client, operator := memtransport.Pair(t, cfp, ofp)
	m := pairing.NewManager(pairing.Config{})
	defer m.Close()
	display, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	hello := encodedMessages(t, proto.Hello{Role: "client", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}})
	script := &closingScriptTransport{Session: operator, ctx: ctx,
		inputSize: len(hello) + len(encodedMessages(t, proto.PairShareA{}, proto.PairConfirmA{}))}
	remembered := false
	result := make(chan error, 1)
	go func() {
		_, err := Run(ctx, exportSession{script, exp}, Options{
			Role: RoleOperator, LocalFP: ofp, DigitalManager: managerResponder{m},
			Remember: func(context.Context, identity.Fingerprint) error { remembered = true; return nil },
		})
		result <- err
	}()
	st, err := client.OpenStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Write(hello); err != nil {
		t.Fatal(err)
	}
	var operatorHello bytes.Buffer
	if _, err = proto.Read(io.TeeReader(st, &operatorHello), proto.MaxFrameLen); err != nil {
		t.Fatal(err)
	}
	a, ya, err := cpace.Start(display.Code, cpace.Binding{
		Exporter: exp, ClientFP: cfp, OperatorFP: ofp,
		HelloClientHash: sha256.Sum256(hello), HelloOperatorHash: sha256.Sum256(operatorHello.Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(st, proto.PairShareA{Value: ya}); err != nil {
		t.Fatal(err)
	}
	yb, err := readPair[proto.PairShareB](st)
	if err != nil {
		t.Fatal(err)
	}
	ta, err := a.Confirm(yb.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(st, proto.PairConfirmA{Value: ta}); err != nil {
		t.Fatal(err)
	}
	if err = client.Close(nil); err != nil {
		t.Fatal(err)
	}
	if err = <-result; !errors.Is(err, ReasonInterrupted) {
		t.Fatalf("operator did not report interruption: %v", err)
	}
	if _, err = proto.Read(&script.output, proto.MaxFrameLen); err != nil {
		t.Fatal(err)
	}
	if _, err = readPair[proto.PairShareB](&script.output); err != nil || script.output.Len() != 0 {
		t.Fatal("expected exactly one operator share and no additional pairing frames")
	}
	if remembered || m.Status().AttemptsLeft != display.AttemptsLeft-1 {
		t.Fatal("operator remembered the peer or refunded the charged attempt")
	}
}

func TestDigitalManagerWithLegacyClient(t *testing.T) {
	for _, saved := range []bool{false, true} {
		token := [16]byte{}
		if !saved {
			token[0] = 1
		}
		client, operator, _, _ := runPair(t, Options{Role: RoleClient, Initiator: true, Token: token, Capabilities: []string{proto.PairDirectCapability}}, Options{
			Role: RoleOperator, DigitalManager: testResponder{refusal: cpace.ReasonBusy},
			Trusted:     func(identity.Fingerprint) bool { return saved },
			AcceptToken: func(_ identity.Fingerprint, received [16]byte) bool { return !saved && received == token },
		})
		if client.PairingOutcome() != Legacy || operator.PairingOutcome() != Legacy || len(client.NegotiatedCapabilities) != 0 {
			t.Fatal("legacy client negotiated numeric pairing")
		}
	}
}

func TestOperatorRejectsUnexpectedPairFrames(t *testing.T) {
	const code = "00001234"
	_, y, err := cpace.Start(code, testBinding)
	if err != nil {
		t.Fatal(err)
	}
	a := proto.PairShareA{Value: y}
	for _, tc := range []struct {
		name   string
		frames []any
		shares int
	}{
		{"confirm first", []any{proto.PairConfirmA{}}, 0},
		{"duplicate share", []any{a, a}, 1},
		{"accepted instead of confirmation", []any{a, proto.PairResult{Accepted: true}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := proto.Hello{Role: "client", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t, append([]any{h}, tc.frames...)...))}
			record := &recordingTransport{Session: ts}
			s, err := Run(testContext(t), exportSession{record, [32]byte{9}}, Options{Initiator: true, Role: RoleOperator, LocalFP: identity.Fingerprint{2}, DigitalManager: testResponder{code: code}, Remember: func(context.Context, identity.Fingerprint) error { t.Fatal("saved without confirmation"); return nil }})
			if s != nil || !errors.Is(err, ReasonInterrupted) {
				t.Fatal(err)
			}
			if _, err := proto.Read(&record.output, proto.MaxFrameLen); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.shares; i++ {
				msg, err := proto.ReadPair(&record.output)
				if _, ok := msg.(proto.PairShareB); err != nil || !ok {
					t.Fatalf("expected response share: %T, %v", msg, err)
				}
			}
			if record.output.Len() != 0 {
				t.Fatal("unexpected extra pairing frame")
			}
		})
	}
}

// testBinding is non-zero because cpace rejects zero exporters and fingerprints.
var testBinding = cpace.Binding{Exporter: [32]byte{2}, ClientFP: [32]byte{3}, OperatorFP: [32]byte{4}}
