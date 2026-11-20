package session

import (
	"bytes"
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/cryptotest"
	"testing/synctest"

	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/pairing"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

const centerCode = "00234567"

type centerResponder struct{ *pairing.SessionResponder }

func (r centerResponder) Begin(ctx context.Context) (DigitalAttempt, error) {
	return r.SessionResponder.Begin(ctx)
}

func centerOptions(role Role) Options {
	o := Options{Role: role, Initiator: role == RoleClient, Center: &CenterBinding{CenterID: "center.one", Session: [16]byte{1}, Hub: identity.Fingerprint{3}}, HoldTunnels: true,
		ConfirmSAS: func(context.Context, string, identity.Fingerprint) (bool, error) { return true, nil },
	}
	if role == RoleClient {
		o.LocalFP = identity.Fingerprint{1}
		o.DigitalCode = centerCode
	} else {
		o.LocalFP = identity.Fingerprint{2}
	}
	return o
}

type runResult struct {
	s   *Session
	err error
}

func runCenter(t *testing.T, c, o Options, wrap func(transport.Session) transport.Session) (runResult, runResult) {
	t.Helper()
	ctx := testContext(t)
	a, b := memtransport.Pair(t, c.LocalFP, o.LocalFP)
	if wrap != nil {
		b = wrap(b)
	}
	done := make(chan runResult, 1)
	go func() { s, err := Run(ctx, exportSession{b, [32]byte{9}}, o); done <- runResult{s, err} }()
	s, err := Run(ctx, exportSession{a, [32]byte{9}}, c)
	return runResult{s, err}, <-done
}

func TestCenterConfirmAndPin(t *testing.T) {
	c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
	r := pairing.NewSessionResponder(centerCode, 3)
	defer r.Close()
	o.DigitalManager = centerResponder{r}
	var checked, pinned identity.Fingerprint
	prompts := 0
	clientSAS := make(chan string, 1)
	c.ConfirmSAS = func(_ context.Context, sas string, peer identity.Fingerprint) (bool, error) {
		if len(sas) != 6 || peer != o.LocalFP {
			t.Error("wrong client prompt")
		}
		clientSAS <- sas
		return true, nil
	}
	o.ConfirmSAS = func(ctx context.Context, sas string, peer identity.Fingerprint) (bool, error) {
		select {
		case reported := <-clientSAS:
			if reported != sas {
				return false, nil
			}
		case <-ctx.Done():
			return false, ctx.Err()
		}
		if checked != peer {
			prompts++
			checked = peer
		}
		return true, nil
	}
	o.Remember = func(_ context.Context, peer identity.Fingerprint) error { pinned = peer; return nil }
	for i, fp := range []identity.Fingerprint{{1}, {1}, {4}} {
		c.LocalFP = fp
		cr, or := runCenter(t, c, o, nil)
		if cr.err != nil || or.err != nil || cr.s.PairingOutcome() != DigitalPaired || or.s.PairingOutcome() != DigitalPaired || pinned != fp || prompts != []int{1, 1, 2}[i] || r.AttemptsLeft() != 2-i {
			t.Fatalf("attempt %d: client %v operator %v prompts %d pin %v left %d", i, cr.err, or.err, prompts, pinned, r.AttemptsLeft())
		}
		for _, s := range []*Session{cr.s, or.s} {
			if !slices.Equal(s.NegotiatedCapabilities, []string{proto.PairCenterCapability}) {
				t.Fatal(s.NegotiatedCapabilities)
			}
			s.ReleaseTunnels()
			if !s.tunnelsHeld.Load() {
				t.Fatal("pairing connection released")
			}
		}
	}
}

func TestCenterDecisionWhileConfirming(t *testing.T) {
	wait := func(ctx context.Context, _ string, _ identity.Fingerprint) (bool, error) {
		<-ctx.Done()
		return false, ctx.Err()
	}
	for _, tc := range []struct {
		name    string
		role    Role
		timeout bool
		want    cpace.Reason
	}{
		{"operator rejects", RoleOperator, false, cpace.ReasonRejected}, {"operator times out", RoleOperator, true, cpace.ReasonConfirmTimeout},
		{"client rejects", RoleClient, false, cpace.ReasonRejected}, {"client times out", RoleClient, true, cpace.ReasonConfirmTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
			r := pairing.NewSessionResponder(centerCode, 1)
			defer r.Close()
			o.DigitalManager = centerResponder{r}
			c.ConfirmSAS, o.ConfirmSAS = wait, wait
			local := &o
			if tc.role == RoleClient {
				local = &c
			}
			if tc.timeout {
				local.ConfirmTimeout = 20 * time.Millisecond
			} else {
				local.ConfirmSAS = func(context.Context, string, identity.Fingerprint) (bool, error) { return false, nil }
			}
			cr, or := runCenter(t, c, o, nil)
			if !errors.Is(cr.err, tc.want) || tc.role == RoleOperator && !errors.Is(or.err, tc.want) || or.err == nil || r.AttemptsLeft() != 0 {
				t.Fatalf("client %v, operator %v", cr.err, or.err)
			}
		})
	}
}

func TestCenterAcceptedBeforeLocalConfirmation(t *testing.T) {
	ctx := testContext(t)
	c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
	o.DigitalManager = testResponder{code: centerCode}
	o.Remember = func(context.Context, identity.Fingerprint) error { return nil }
	a, b := memtransport.Pair(t, c.LocalFP, o.LocalFP)
	closed := make(chan struct{})
	c.ConfirmSAS = func(ctx context.Context, _ string, _ identity.Fingerprint) (bool, error) {
		select {
		case <-closed:
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, exportSession{b, [32]byte{9}}, o)
		_ = b.Close(nil)
		close(closed)
		done <- err
	}()
	s, err := Run(ctx, exportSession{a, [32]byte{9}}, c)
	if opErr := <-done; err != nil || opErr != nil || s.PairingOutcome() != DigitalPaired {
		t.Fatalf("client %v operator %v", err, opErr)
	}
}

func TestCenterFailedAttempts(t *testing.T) {
	c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
	r := pairing.NewSessionResponder(centerCode, 2)
	defer r.Close()
	o.DigitalManager = centerResponder{r}
	charges := 0
	c.BeginClientAttempt = func() bool { charges++; return true }
	o.Remember = func(context.Context, identity.Fingerprint) error { t.Error("saved a failed attempt"); return nil }
	for i, tc := range []string{"changed hello", "wrong code", "exhausted"} {
		var wrap func(transport.Session) transport.Session
		want := cpace.ReasonConfirmation
		switch tc {
		case "changed hello":
			wrap = func(b transport.Session) transport.Session { return alteredHello{b} }
		case "wrong code":
			c.DigitalCode = "12345678"
		case "exhausted":
			want = cpace.ReasonBudgetExhausted
		}
		cr, or := runCenter(t, c, o, wrap)
		if !errors.Is(cr.err, want) || !errors.Is(or.err, want) || r.AttemptsLeft() != max(0, 1-i) || charges != i+1 {
			t.Fatalf("%s: client %v operator %v left %d charges %d", tc, cr.err, or.err, r.AttemptsLeft(), charges)
		}
	}

	c.BeginClientAttempt = func() bool { return false }
	left := r.AttemptsLeft()
	cr, or := runCenter(t, c, o, nil)
	// The client abandons right after Hello. Depending on timing the operator
	// fails while still writing Hello or while waiting for message 11; either
	// way it must not succeed or consume an attempt.
	if !errors.Is(cr.err, cpace.ReasonBudgetExhausted) || or.err == nil || r.AttemptsLeft() != left {
		t.Fatalf("client budget: %v %v left %d", cr.err, or.err, r.AttemptsLeft())
	}
	c.BeginClientAttempt = func() bool { charges++; return true }
	o.Center = nil
	cr, _ = runCenter(t, c, o, nil)
	if !errors.Is(cr.err, ReasonUnsupported) || charges != 3 {
		t.Fatalf("fallback: %v, charges %d", cr.err, charges)
	}
}

func TestCenterCommitAdmission(t *testing.T) {
	h := proto.Hello{Role: "client", ProtoVersion: proto.Version, Capabilities: []string{proto.PairCenterCapability}}
	for _, tc := range []struct {
		name     string
		messages []any
		left     int
	}{
		{"disconnect before Tb", []any{proto.PakeCommit{}}, 0},
		{"wrong order", []any{proto.PakeShareA{}}, 1},
		{"duplicate commit", []any{proto.PakeCommit{}, proto.PakeCommit{}}, 0},
		{"wrong commitment", []any{proto.PakeCommit{}, proto.PakeShareA{}}, 0},
		{"bad commit length", nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := pairing.NewSessionResponder(centerCode, 1)
			defer r.Close()
			input := encodedMessages(t, append([]any{h}, tc.messages...)...)
			if tc.messages == nil {
				input = append(input, 0, 0, 1, 1)
			}
			ts := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
			ts.stream = &handshakeStream{owner: ts, input: bytes.NewReader(input)}
			record := &recordingTransport{Session: ts}
			o := centerOptions(RoleOperator)
			o.DigitalManager = centerResponder{r}
			o.Initiator = true
			_, err := Run(t.Context(), exportSession{record, [32]byte{9}}, o)
			if err == nil || r.AttemptsLeft() != tc.left {
				t.Fatalf("err %v left %d", err, r.AttemptsLeft())
			}
			if _, err := proto.Read(&record.output, proto.MaxFrameLen); err != nil {
				t.Fatal(err)
			}
			if tc.left == 0 {
				if _, err := readPair[proto.PakeShareB](&record.output); err != nil {
					t.Fatal(err)
				}
			}
			if record.output.Len() != 0 {
				t.Fatal("unexpected pairing decision")
			}
		})
	}
}

func TestCenterDataGate(t *testing.T) {
	c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
	c.DigitalCode = ""
	c.AllowLinkTest, o.AllowLinkTest = true, true
	o.Pinned = func(fp identity.Fingerprint) bool { return fp == c.LocalFP }
	c.OnTunnel = func(_ context.Context, _ proto.OpenTunnel, st transport.Stream) { _, _ = st.Write([]byte("released")) }
	cs, os, _, wire := runPair(t, c, o)
	for _, cap := range []string{proto.HubTicketCapability, proto.PairCenterCapability} {
		if slices.Contains(cs.NegotiatedCapabilities, cap) {
			t.Fatal("data client advertised pairing capability")
		}
	}
	h := proto.OpenTunnel{Kind: "tcp", Target: "local"}
	if _, err := os.OpenTunnel(t.Context(), h); !errors.Is(err, ErrTunnelsHeld) {
		t.Fatal(err)
	}
	if _, err := cs.TestLink(t.Context(), linktest.Options{}); !errors.Is(err, ErrTunnelsHeld) {
		t.Fatal(err)
	}
	for _, hdr := range []proto.OpenTunnel{h, {Kind: linktest.Capability}} {
		st, err := wire.OpenStream(testContext(t))
		if err != nil {
			t.Fatal(err)
		}
		_ = proto.Write(st, hdr)
		var b [1]byte
		if n, err := st.Read(b[:]); n != 0 || err == nil {
			t.Fatal("inbound stream passed gate")
		}
		_ = st.Close()
	}
	os.ReleaseTunnels()
	cs.ReleaseTunnels()
	cs.ReleaseTunnels()
	st, err := os.OpenTunnel(testContext(t), h)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	data, err := io.ReadAll(st)
	if err != nil || string(data) != "released" {
		t.Fatal(string(data), err)
	}
	// A late not_paired is still terminal after ordinary control traffic.
	if err := os.Send(proto.Log{Msg: "data"}); err != nil {
		t.Fatal(err)
	}
	_ = os.Send(proto.PairDecision{Reason: "not_paired"})
	<-cs.Done()
	if !errors.Is(cs.Err(), cpace.ReasonNotPaired) {
		t.Fatal(cs.Err())
	}
}

func TestCenterUnpinnedData(t *testing.T) {
	c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
	c.DigitalCode = ""
	o.Trusted = func(identity.Fingerprint) bool { return true }
	cr, or := runCenter(t, c, o, nil)
	if cr.err != nil {
		t.Fatal(cr.err)
	}
	<-cr.s.Done()
	if !errors.Is(cr.s.Err(), cpace.ReasonNotPaired) || !errors.Is(or.err, cpace.ReasonNotPaired) {
		t.Fatalf("%v %v", cr.s.Err(), or.err)
	}
}

func TestCenterRequiresHook(t *testing.T) {
	for _, role := range []Role{RoleClient, RoleOperator} {
		o := centerOptions(role)
		o.ConfirmSAS = nil
		_, err := Run(t.Context(), nil, o)
		if err == nil {
			t.Fatal("missing hook accepted")
		}
	}
}

func TestCenterSeparateAuthenticatedLinksShowDifferentSAS(t *testing.T) {
	cryptotest.SetGlobalRandom(t, 7)
	// The intermediary knows the code and terminates both authenticated links.
	// Complete one leg at a time so the scalar draws have a fixed order.
	var observed []string
	capture := func(_ context.Context, sas string, _ identity.Fingerprint) (bool, error) {
		observed = append(observed, sas)
		return true, nil
	}
	for _, role := range []Role{RoleClient, RoleOperator} {
		c, o := centerOptions(RoleClient), centerOptions(RoleOperator)
		if role == RoleClient {
			o.LocalFP = identity.Fingerprint{3}
			c.ConfirmSAS = capture
		} else {
			c.LocalFP = identity.Fingerprint{3}
			o.ConfirmSAS = capture
		}
		o.DigitalManager = testResponder{code: centerCode}
		o.Remember = func(context.Context, identity.Fingerprint) error { return nil }
		cr, or := runCenter(t, c, o, nil)
		if cr.err != nil || or.err != nil {
			t.Fatalf("%v %v", cr.err, or.err)
		}
	}
	if len(observed) != 2 || observed[0] == observed[1] {
		t.Fatal("independent links showed the same SAS", strings.Join(observed, ","))
	}
}

// Transport shutdown and an accept error may both be visible before the final
// control Read returns previously buffered data.
type bufferedDecisionTransport struct {
	*handshakeTransport
	accepted chan struct{}
}

func (t *bufferedDecisionTransport) AcceptStream(ctx context.Context) (transport.Stream, error) {
	close(t.accepted)
	return nil, io.EOF
}

func TestCenterBufferedNotPairedAfterTransportEOF(t *testing.T) {
	base := &handshakeTransport{done: make(chan struct{}), closed: make(chan struct{})}
	ts := &bufferedDecisionTransport{base, make(chan struct{})}
	h := proto.Hello{Role: "operator", ProtoVersion: proto.Version, Capabilities: []string{proto.PairCenterCapability}}
	base.stream = &handshakeStream{owner: base, input: bytes.NewReader(encodedMessages(t, h, proto.PairDecision{Reason: "not_paired"})), afterRead: func() {
		<-ts.accepted
		base.terminate(io.EOF)
	}}
	c := centerOptions(RoleClient)
	c.DigitalCode = ""
	s, err := Run(testContext(t), ts, c)
	if err != nil {
		t.Fatal(err)
	}
	<-s.Done()
	if !errors.Is(s.Err(), cpace.ReasonNotPaired) {
		t.Fatal(s.Err())
	}
}

func TestCenterSASDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		o := centerOptions(RoleOperator)
		o.ConfirmTimeout = 3 * time.Minute
		start := time.Now()
		o.ConfirmSAS = func(ctx context.Context, _ string, _ identity.Fingerprint) (bool, error) {
			<-ctx.Done()
			return false, ctx.Err()
		}
		s := Session{ts: &handshakeTransport{}, opts: o}
		if err := s.confirmCenter(t.Context(), "123456"); !errors.Is(err, cpace.ReasonConfirmTimeout) || time.Since(start) != o.ConfirmTimeout {
			t.Fatalf("confirmation returned %v after %v", err, time.Since(start))
		}
	})
}
