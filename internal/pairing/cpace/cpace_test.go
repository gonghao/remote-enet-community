package cpace

import (
	"bytes"
	"testing"
)

const exampleCode = "00001234"

func testBinding() Binding {
	return Binding{
		Exporter:          [32]byte{2},
		ClientFP:          [32]byte{3},
		OperatorFP:        [32]byte{4},
		HelloClientHash:   [32]byte{5},
		HelloOperatorHash: [32]byte{6},
	}
}

func startTest(t *testing.T, b Binding) (*Initiator, [32]byte) {
	t.Helper()
	i, a, err := Start(exampleCode, b)
	if err != nil {
		t.Fatal(err)
	}
	return i, a
}
func exchangeTest(t *testing.T, opCode string, b Binding) (*Initiator, *Responder, [32]byte) {
	t.Helper()
	i, a := startTest(t, testBinding())
	r, y, err := Respond(opCode, b, a)
	if err != nil {
		t.Fatal(err)
	}
	tag, err := i.Confirm(y)
	if err != nil {
		t.Fatal(err)
	}
	return i, r, tag
}
func TestSingleUse(t *testing.T) {
	i, r, ta := exchangeTest(t, exampleCode, testBinding())
	copyR := *r
	tb, err := r.VerifyAndConfirm(ta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := copyR.VerifyAndConfirm(ta); err == nil {
		t.Fatal("reused responder")
	}
	if err := i.Verify(tb); err != nil {
		t.Fatal(err)
	}
	if err := i.Verify(tb); err == nil {
		t.Fatal("reused initiator")
	}
	if _, err := i.Confirm([32]byte{}); err == nil {
		t.Fatal("repeated confirm")
	}
	i, r, ta = exchangeTest(t, exampleCode, testBinding())
	if err := i.Verify(ta); err != ReasonConfirmation {
		t.Fatal("accepted reflected tag")
	}
	tb, err = r.VerifyAndConfirm(ta)
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Verify(tb); err == nil {
		t.Fatal("retried after failed verify")
	}
	i, a := startTest(t, testBinding())
	if err := i.Verify(ta); err == nil {
		t.Fatal("accepted early verify")
	}
	if _, err := i.Confirm(a); err == nil {
		t.Fatal("retried after early verify")
	}
}
func TestWrongCodeAndBinding(t *testing.T) {
	checkMismatch := func(code string, b Binding) {
		t.Helper()
		i, r, ta := exchangeTest(t, code, b)
		candidate := r.s.pending.operator
		if err := i.Verify(candidate); err != ReasonConfirmation {
			t.Fatal("initiator accepted mismatched exchange")
		}
		if result, err := r.VerifyAndConfirm(ta); err != ReasonConfirmation || result != [32]byte{} {
			t.Fatal("responder accepted mismatched exchange")
		}
		if _, err := r.VerifyAndConfirm(ta); err == nil {
			t.Fatal("retried failed verification")
		}
	}
	checkMismatch("00001235", testBinding())
	b := testBinding()
	for _, field := range [][]byte{b.Exporter[:], b.ClientFP[:], b.OperatorFP[:], b.HelloClientHash[:], b.HelloOperatorHash[:]} {
		for n := range field {
			field[n] ^= 1
			checkMismatch(exampleCode, b)
			field[n] ^= 1
		}
	}
}
func TestRoleBinding(t *testing.T) {
	v := readVector(t, "application")
	ci := v.bytes(t, "CI")
	for _, role := range []string{"client", "operator"} {
		offset := bytes.Index(ci, []byte(role))
		if offset < 0 {
			t.Fatal("missing role")
		}
		for n := range len(role) {
			changed := bytes.Clone(ci)
			changed[offset+n] ^= 1
			g, err := generator(v.bytes(t, "PRS"), changed, v.bytes(t, "sid"))
			if err != nil {
				t.Fatal(err)
			}
			// Model a peer using a different role in its generator context.
			y := multiply(v.scalar(t, "yb"), g)
			k, err := sharedPoint(v.scalar(t, "ya"), y[:])
			if err != nil {
				t.Fatal(err)
			}
			b := v.binding(t)
			a := [32]byte(v.bytes(t, "Ya"))
			received := confirmationTags(b, k, a, y)
			peerShared, err := sharedPoint(v.scalar(t, "yb"), a[:])
			if err != nil {
				t.Fatal(err)
			}
			peer := confirmationTags(b, peerShared, a, y)
			r := Responder{&responderState{pending: &peer}}
			if _, err := r.VerifyAndConfirm(received.client); err != ReasonConfirmation {
				t.Fatal("accepted mutated role")
			}
		}
	}
}
func TestInvalidCode(t *testing.T) {
	for _, code := range []string{"", "1234567", "123456789", "1234567a", "１２３４", "1234 5678"} {
		if _, _, err := Start(code, testBinding()); err != errCode {
			t.Fatal("invalid code accepted")
		}
		if _, _, err := Respond(code, testBinding(), [32]byte{}); err != errCode {
			t.Fatal("invalid code accepted")
		}
	}
}

func TestInvalidBinding(t *testing.T) {
	valid := testBinding()
	_, a := startTest(t, valid)
	for _, b := range []Binding{
		{},
		{ClientFP: valid.ClientFP, OperatorFP: valid.OperatorFP},
		{Exporter: valid.Exporter, OperatorFP: valid.OperatorFP},
		{Exporter: valid.Exporter, ClientFP: valid.ClientFP},
	} {
		if state, share, err := Start(exampleCode, b); err != errBinding || state != nil || share != [32]byte{} {
			t.Fatal("Start did not reject missing binding data")
		}
		if state, share, err := Respond(exampleCode, b, a); err != errBinding || state != nil || share != [32]byte{} {
			t.Fatal("Respond did not reject missing binding data")
		}
	}
}

func TestConcurrentConfirmation(t *testing.T) {
	i, r, ta := exchangeTest(t, exampleCode, testBinding())
	type result struct {
		tag [32]byte
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() { tag, err := r.VerifyAndConfirm(ta); results <- result{tag, err} }()
	}
	first, second := <-results, <-results
	if first.err != nil {
		first, second = second, first
	}
	if first.err != nil || second.err != errState || i.Verify(first.tag) != nil {
		t.Fatal("expected exactly one successful confirmation")
	}
}
