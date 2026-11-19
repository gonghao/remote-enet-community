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
func (a testAttempt) Respond(b cpace.Binding, y [64]byte) (*cpace.Responder, [32]byte, error) {
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
	return alteredHelloReader(st), nil
}

func (s alteredHello) OpenStream(ctx context.Context) (transport.Stream, error) {
	st, err := s.Session.OpenStream(ctx)
	if err != nil {
		return nil, err
	}
	return alteredHelloReader(st), nil
}

type helloReader struct {
	io.Reader
	stream transport.Stream
}

func alteredHelloReader(st transport.Stream) transport.Stream {
	return &prefixedStream{st, &helloReader{stream: st}}
}
func (r *helloReader) Read(p []byte) (int, error) {
	if r.Reader == nil {
		st := r.stream
		var prefix [4]byte
		if _, err := io.ReadFull(st, prefix[:]); err != nil {
			return 0, err
		}
		raw := make([]byte, binary.BigEndian.Uint32(prefix[:]))
		if _, err := io.ReadFull(st, raw); err != nil {
			return 0, err
		}
		frame := binary.BigEndian.AppendUint32(nil, uint32(len(raw)+1))
		frame = append(frame, raw[0], 0x18)
		frame = append(frame, raw[1:]...)
		r.Reader = io.MultiReader(bytes.NewReader(frame), st)
	}
	return r.Reader.Read(p)
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
		{"success", ""}, {"wrong code", cpace.ReasonConfirmation}, {"SAS rejected", cpace.ReasonConfirmation}, {"altered operator hello", cpace.ReasonConfirmation}, {"altered hello", cpace.ReasonConfirmation}, {"exporter mismatch", cpace.ReasonConfirmation}, {"save failed", cpace.ReasonStoreFailed}, {"no code", cpace.ReasonUnavailable},
		{"lost decision", ReasonInterrupted}, {"commit canceled", ReasonInterrupted}, {"commit deadline", ReasonInterrupted}, {"save deadline", cpace.ReasonStoreFailed},
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
			case "altered operator hello":
				a = alteredHello{a}
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
				case "lost decision":
					return b.Close(nil)
				case "save failed":
					return errors.New(code)
				case "save deadline":
					return context.DeadlineExceeded
				}
				return nil
			}}
			if tc.name == "SAS rejected" {
				o.ConfirmSAS = func(_ context.Context, sas string, peer identity.Fingerprint) (bool, error) {
					if len(sas) != 6 || peer != cfp {
						t.Error("invalid SAS prompt")
					}
					return false, nil
				}
			}
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
				for role, failure := range map[Role]error{RoleClient: err, RoleOperator: op.err} {
					want := tc.want
					if tc.name == "SAS rejected" && role == RoleClient {
						want = ReasonInterrupted
					}
					if !errors.Is(failure, want) || strings.Contains(failure.Error(), code) {
						t.Fatalf("%s: got %v, want %v", role, failure, want)
					}
				}
				if cs != nil || op.s != nil {
					t.Fatal("failed handshake returned session")
				}
			}
			wantSaves := 0
			if tc.want == "" || tc.want == cpace.ReasonStoreFailed || tc.name == "lost decision" {
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
	_, share, err := cpace.Respond(code, testBinding, [64]byte{})
	if err != nil {
		t.Fatal(err)
	}
	shareB := proto.PakeShareB{Value: share}
	for _, tc := range []struct {
		name             string
		replies          []any
		want             cpace.Reason
		shares, confirms int
	}{
		{"unsupported", nil, ReasonUnsupported, 0, 0},
		{"missing exporter", nil, ReasonInterrupted, 0, 0},
		{"early accepted", []any{proto.PairDecision{Accepted: true}}, ReasonInterrupted, 1, 0},
		{"late accepted", []any{shareB, proto.PairDecision{Accepted: true}}, ReasonInterrupted, 1, 1},
		{"lost before Tb", []any{shareB}, cpace.ReasonConfirmation, 1, 1},
		{"wrong order", []any{proto.PakeConfirmB{}}, ReasonInterrupted, 1, 0},
		{"duplicate share", []any{shareB, shareB}, ReasonInterrupted, 1, 1},
		{"wrong tag", []any{shareB, proto.PakeConfirmB{}}, cpace.ReasonConfirmation, 1, 1},
		{"unknown reason", []any{proto.PairDecision{Reason: code}}, ReasonInterrupted, 1, 0},
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
				case proto.PakeCommit:
					shares++
				case proto.PakeShareA:
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
			input: bytes.NewReader(encodedMessages(t, hello, proto.PairDecision{Reason: string(cpace.ReasonExpired)})),
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
		inputSize: len(hello) + len(encodedMessages(t, proto.PakeCommit{}, proto.PakeShareA{}, proto.PakeConfirmA{}))}
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
		Capability: proto.PairDirectCapability,
		Exporter:   exp, ClientFP: cfp, OperatorFP: ofp,
		HelloClientHash: sha256.Sum256(hello), HelloOperatorHash: sha256.Sum256(operatorHello.Bytes()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(st, proto.PakeCommit{Value: ya}); err != nil {
		t.Fatal(err)
	}
	yb, err := readPair[proto.PakeShareB](st)
	if err != nil {
		t.Fatal(err)
	}
	share, err := a.Reveal(yb.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(st, proto.PakeShareA{Value: share}); err != nil {
		t.Fatal(err)
	}
	tb, err := readPair[proto.PakeConfirmB](st)
	if err != nil {
		t.Fatal(err)
	}
	ta, _, err := a.Finish(tb.Value)
	if err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(st, proto.PakeConfirmA{Value: ta}); err != nil {
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
	if _, err = readPair[proto.PakeShareB](&script.output); err != nil {
		t.Fatal(err)
	}
	if _, err = readPair[proto.PakeConfirmB](&script.output); err != nil || script.output.Len() != 0 {
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
	a := proto.PakeCommit{Value: y}
	for _, tc := range []struct {
		name   string
		frames []any
		shares int
	}{
		{"confirm first", []any{proto.PakeConfirmA{}}, 0},
		{"duplicate share", []any{a, a}, 1},
		{"accepted instead of confirmation", []any{a, proto.PairDecision{Accepted: true}}, 1},
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
				if _, ok := msg.(proto.PakeShareB); err != nil || !ok {
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
var testBinding = cpace.Binding{Capability: proto.PairDirectCapability, Exporter: [32]byte{2}, ClientFP: [32]byte{3}, OperatorFP: [32]byte{4}}

func TestDirectAdmissionBudget(t *testing.T) {
	for _, reason := range []cpace.Reason{cpace.ReasonUnavailable, cpace.ReasonExpired, cpace.ReasonBusy, cpace.ReasonBudgetExhausted, cpace.ReasonRateLimited, ""} {
		t.Run(string(reason), func(t *testing.T) {
			now := time.Now()
			cfg := pairing.Config{Now: func() time.Time { return now }}
			if reason == cpace.ReasonBudgetExhausted {
				cfg.PerCode = 1
			}
			if reason == cpace.ReasonRateLimited {
				cfg.GlobalLimit = 1
			}
			m := pairing.NewManager(cfg)
			defer m.Close()
			if reason != cpace.ReasonUnavailable {
				if _, err := m.Issue(); err != nil {
					t.Fatal(err)
				}
			}
			switch reason {
			case cpace.ReasonExpired:
				now = now.Add(time.Hour)
			case cpace.ReasonBusy, cpace.ReasonBudgetExhausted, cpace.ReasonRateLimited:
				owner, err := m.Begin(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if _, _, err = owner.Respond(testBinding, [64]byte{}); err != nil {
					t.Fatal(err)
				}
				if reason != cpace.ReasonBusy {
					owner.Finish()
				}
			}
			before := m.Status()
			h := proto.Hello{Role: "client", ProtoVersion: proto.Version, Capabilities: []string{proto.PairDirectCapability}}
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(encodedMessages(t, h, proto.PakeCommit{}))}
			record := &recordingTransport{Session: ts}
			sess, err := Run(t.Context(), exportSession{record, [32]byte{1}}, Options{Initiator: true, Role: RoleOperator, LocalFP: testBinding.OperatorFP, DigitalManager: managerResponder{m}})
			if err == nil || sess != nil {
				t.Fatal("unexpected success")
			}
			if _, err := proto.Read(&record.output, proto.MaxFrameLen); err != nil {
				t.Fatal(err)
			}
			msg, err := proto.ReadPair(&record.output)
			if err != nil || record.output.Len() != 0 {
				t.Fatal("unexpected pairing frames", err)
			}
			if reason == "" {
				if _, ok := msg.(proto.PakeShareB); !ok || m.Status().AttemptsLeft != before.AttemptsLeft-1 {
					t.Fatal("disconnect before Tb refunded attempt")
				}
			} else if msg != (proto.PairDecision{Reason: string(reason)}) || m.Status() != before {
				t.Fatal("incorrect rejection or budget", msg, m.Status())
			}
		})
	}
}

func checkPairIOFailure[T any](t *testing.T, msg T, sendReason, readReason cpace.Reason) {
	t.Helper()
	reader, writer := io.Pipe()
	reader.Close()
	defer writer.Close()
	s := Session{control: &recordedStream{writer: writer}}
	if err := s.sendPair(msg); !errors.Is(err, sendReason) {
		t.Errorf("sending %T: got %v, want %v", msg, err, sendReason)
	}
	if _, err := readPair[T](bytes.NewReader(nil)); !errors.Is(err, readReason) {
		t.Errorf("reading %T: got %v, want %v", msg, err, readReason)
	}
}

func TestPairIOFailureByStage(t *testing.T) {
	checkPairIOFailure(t, proto.PakeCommit{}, ReasonInterrupted, ReasonInterrupted)
	checkPairIOFailure(t, proto.PakeShareB{}, cpace.ReasonConfirmation, ReasonInterrupted)
	checkPairIOFailure(t, proto.PakeShareA{}, cpace.ReasonConfirmation, cpace.ReasonConfirmation)
	checkPairIOFailure(t, proto.PakeConfirmB{}, cpace.ReasonConfirmation, cpace.ReasonConfirmation)
	checkPairIOFailure(t, proto.PakeConfirmA{}, ReasonInterrupted, cpace.ReasonConfirmation)
	checkPairIOFailure(t, proto.PairDecision{}, ReasonInterrupted, ReasonInterrupted)
}

type corruptCommitTransport struct {
	transport.Session
	offset  int
	altered bool
	output  bytes.Buffer
}

func (ts *corruptCommitTransport) AcceptStream(ctx context.Context) (transport.Stream, error) {
	st, err := ts.Session.AcceptStream(ctx)
	if err != nil {
		return nil, err
	}
	reader := &corruptCommitReader{Reader: st, ts: ts}
	return &recordedStream{&prefixedStream{st, reader}, io.MultiWriter(&ts.output, st)}, nil
}

type corruptCommitReader struct {
	io.Reader
	ts *corruptCommitTransport
}

func (r *corruptCommitReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if !r.ts.altered {
		if r.ts.offset < n {
			p[r.ts.offset] ^= 1
			r.ts.altered = true
		} else {
			r.ts.offset -= n
		}
	}
	return n, err
}

func TestDigitalCommitMismatchClosesWithoutDecision(t *testing.T) {
	ctx := testContext(t)
	cfp, ofp := identity.Fingerprint{1}, identity.Fingerprint{2}
	client, operator := memtransport.Pair(t, cfp, ofp)
	manager := pairing.NewManager(pairing.Config{})
	defer manager.Close()
	display, err := manager.Issue()
	if err != nil {
		t.Fatal(err)
	}
	hello := proto.Hello{ProtoVersion: proto.Version, Role: "client", Capabilities: []string{proto.PairDirectCapability}}
	// Flip the last commitment byte in transit, preserving its canonical CBOR and the subsequent opening.
	wire := &corruptCommitTransport{Session: operator, offset: len(encodedMessages(t, hello, proto.PakeCommit{})) - 1}
	exporter := [32]byte{9}
	saved := false
	result := make(chan error, 1)
	go func() {
		_, err := Run(ctx, exportSession{wire, exporter}, Options{
			Role: RoleOperator, LocalFP: ofp, DigitalManager: managerResponder{manager},
			Remember: func(context.Context, identity.Fingerprint) error { saved = true; return nil },
		})
		result <- err
	}()
	cs, clientErr := Run(ctx, exportSession{client, exporter}, Options{Initiator: true, Role: RoleClient, LocalFP: cfp, DigitalCode: display.Code})
	operatorErr := <-result
	if cs != nil || !errors.Is(clientErr, cpace.ReasonConfirmation) || !errors.Is(operatorErr, cpace.ReasonConfirmation) {
		t.Fatalf("client %v, operator %v", clientErr, operatorErr)
	}
	select {
	case <-operator.Done():
	default:
		t.Fatal("operator kept the transport open")
	}
	if !wire.altered || saved || manager.Status().AttemptsLeft != display.AttemptsLeft-1 {
		t.Fatal("commit mismatch did not consume exactly one attempt without saving trust")
	}
	if msg, err := proto.Read(&wire.output, proto.MaxFrameLen); err != nil {
		t.Fatal(err)
	} else if _, ok := msg.(proto.Hello); !ok {
		t.Fatalf("expected Hello, got %T", msg)
	}
	if _, err := readPair[proto.PakeShareB](&wire.output); err != nil {
		t.Fatal(err)
	}
	if wire.output.Len() != 0 {
		t.Fatalf("unexpected output after Yb: %x", wire.output.Bytes())
	}
}
