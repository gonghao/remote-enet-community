package cpace

import "testing"

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

func startTest(t *testing.T, b Binding) (*Initiator, [64]byte) {
	t.Helper()
	i, a, err := Start(exampleCode, b)
	if err != nil {
		t.Fatal(err)
	}
	return i, a
}
func exchangeTest(t *testing.T, code string, b Binding) (*Initiator, *Responder, [64]byte) {
	t.Helper()
	i, c := startTest(t, testBinding())
	r, y, err := Respond(code, b, c)
	if err != nil {
		t.Fatal(err)
	}
	a, err := i.Reveal(y)
	if err != nil {
		t.Fatal(err)
	}
	tb, err := r.Open(a)
	if err != nil {
		t.Fatal(err)
	}
	return i, r, tb
}
func TestCommitMismatch(t *testing.T) {
	i, c := startTest(t, testBinding())
	c[0] ^= 1
	r, y, err := Respond(exampleCode, testBinding(), c)
	if err != nil {
		t.Fatal(err)
	}
	a, err := i.Reveal(y)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Open(a); err != ReasonConfirmation {
		t.Fatal(err)
	}
}
func TestSingleUse(t *testing.T) {
	i, r, tb := exchangeTest(t, exampleCode, testBinding())
	copyI := *i
	ta, sas, err := i.Finish(tb)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := copyI.Finish(tb); err != errState {
		t.Fatal(err)
	}
	results := make(chan error, 2)
	for range 2 {
		go func() {
			got, err := r.Verify(ta)
			if err == nil && got != sas {
				t.Error("SAS mismatch")
			}
			results <- err
		}()
	}
	first, second := <-results, <-results
	if !(first == nil && second == errState || second == nil && first == errState) {
		t.Fatal(first, second)
	}
}
func TestWrongCodeAndBinding(t *testing.T) {
	check := func(code string, b Binding) {
		t.Helper()
		i, r, tb := exchangeTest(t, code, b)
		if _, _, err := i.Finish(tb); err != ReasonConfirmation {
			t.Fatal(err)
		}
		if _, err := r.Verify(tb); err != ReasonConfirmation {
			t.Fatal(err)
		}
	}
	check("00001235", testBinding())
	b := testBinding()
	for _, field := range [][]byte{b.Exporter[:], b.ClientFP[:], b.OperatorFP[:], b.HelloClientHash[:], b.HelloOperatorHash[:]} {
		for n := range field {
			field[n] ^= 1
			check(exampleCode, b)
			field[n] ^= 1
		}
	}
}
func TestInvalidCode(t *testing.T) {
	for _, code := range []string{"", "1234567", "123456789", "1234567a", "１２３４", "1234 5678"} {
		if _, _, err := Start(code, testBinding()); err != errCode {
			t.Fatal("invalid code accepted")
		}
		if _, _, err := Respond(code, testBinding(), [64]byte{}); err != errCode {
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
		if state, share, err := Start(exampleCode, b); err != errBinding || state != nil || share != [64]byte{} {
			t.Fatal("Start did not reject missing binding data")
		}
		if state, share, err := Respond(exampleCode, b, a); err != errBinding || state != nil || share != [32]byte{} {
			t.Fatal("Respond did not reject missing binding data")
		}
	}
}

func TestOutOfOrderAndReflectedConfirmation(t *testing.T) {
	b := testBinding()
	i, c := startTest(t, b)
	r, y, err := Respond(exampleCode, b, c)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = i.Finish([64]byte{}); err != errState {
		t.Fatal(err)
	}
	if _, err = i.Reveal(y); err != errState {
		t.Fatal(err)
	}
	if _, err = r.Verify([64]byte{}); err != errState {
		t.Fatal(err)
	}
	if _, err = r.Open([32]byte{}); err == nil {
		t.Fatal("reused invalidated state")
	}
	i, r, tb := exchangeTest(t, exampleCode, b)
	if _, err = r.Verify(tb); err != ReasonConfirmation {
		t.Fatal("accepted reflected Tb")
	}
	ta, _, err := i.Finish(tb)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Verify(ta); err != errState {
		t.Fatal("retried failed verification")
	}
}
