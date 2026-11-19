package pairing

import (
	"context"
	"errors"
	"regexp"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"remote-enet/internal/pairing/cpace"
)

func issue(t *testing.T, m *Manager) Display {
	t.Helper()
	d, err := m.Issue()
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile("^[0-9]{8}$").MatchString(d.Code) {
		t.Fatal("invalid code format")
	}
	return d
}
func begin(t *testing.T, m *Manager, ctx context.Context) *Attempt {
	t.Helper()
	a, err := m.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
func verify(a *Attempt, code string) error {
	b := testBinding
	client, x, err := cpace.Start(code, b)
	if err != nil {
		return err
	}
	server, y, err := a.Respond(b, x)
	if err != nil {
		return err
	}
	tag, err := client.Confirm(y)
	if err != nil {
		return err
	}
	_, err = server.VerifyAndConfirm(tag)
	return err
}
func wantErr(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestExpiry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(Config{})
		defer m.Close()
		d := issue(t, m)
		a := begin(t, m, t.Context())
		time.Sleep(5*time.Minute - time.Nanosecond)
		if err := verify(a, d.Code); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if a.Context().Err() == nil {
			t.Fatal("expiry did not cancel handshake")
		}
		wantErr(t, a.Commit(nil), cpace.ReasonExpired)
		_, err := m.Begin(t.Context())
		wantErr(t, err, cpace.ReasonExpired)
	})
}

func TestRollingBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(Config{})
		defer m.Close()
		for batch := range 4 {
			d := issue(t, m)
			for i := range 5 {
				a := begin(t, m, t.Context())
				if i == 4 && batch == 3 {
					if err := verify(a, d.Code); err != nil {
						t.Fatal(err)
					}
					_, err := m.Issue()
					wantErr(t, err, cpace.ReasonRateLimited)
					_, err = m.Begin(t.Context())
					wantErr(t, err, cpace.ReasonRateLimited)
					a.Abort()
				} else {
					_, _, err := a.Respond(testBinding, [32]byte{})
					wantErr(t, err, cpace.ReasonConfirmation)
				}
				if m.Status().AttemptsLeft != 4-i {
					t.Fatal("incorrect per-code budget")
				}
			}
			_, err := m.Begin(t.Context())
			wantErr(t, err, cpace.ReasonExhausted)
			if m.Status().Reason != cpace.ReasonExhausted {
				t.Fatal("retired code should be exhausted")
			}
			time.Sleep(time.Minute)
		}
		time.Sleep(11*time.Minute - time.Nanosecond)
		_, err := m.Issue()
		wantErr(t, err, cpace.ReasonRateLimited)
		time.Sleep(time.Nanosecond)
		_, err = m.Begin(t.Context())
		wantErr(t, err, cpace.ReasonExhausted)
		issue(t, m)
		for range 5 {
			a := begin(t, m, t.Context())
			_, _, err := a.Respond(testBinding, [32]byte{})
			wantErr(t, err, cpace.ReasonConfirmation)
		}
		_, err = m.Issue()
		wantErr(t, err, cpace.ReasonRateLimited)
	})
}

func TestTerminalReasonAtGlobalLimit(t *testing.T) {
	for _, reason := range []cpace.Reason{cpace.ReasonExpired, cpace.ReasonUnavailable} {
		t.Run(string(reason), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := NewManager(Config{GlobalLimit: 1})
				defer m.Close()
				display := issue(t, m)
				a := begin(t, m, t.Context())
				if err := verify(a, display.Code); err != nil {
					t.Fatal(err)
				}
				_, err := m.Begin(t.Context())
				wantErr(t, err, cpace.ReasonRateLimited)
				if reason == cpace.ReasonExpired {
					time.Sleep(time.Until(display.ExpiresAt))
				} else {
					wantErr(t, m.Revoke(t.Context(), display.Generation), nil)
				}
				_, err = m.Begin(t.Context())
				wantErr(t, err, reason)
				_, err = m.Issue()
				wantErr(t, err, cpace.ReasonRateLimited)
			})
		})
	}
}

func TestLastGuessAndConcurrentRedemption(t *testing.T) {
	m := NewManager(Config{PerCode: 1, GlobalLimit: 1})
	defer m.Close()
	d := issue(t, m)
	start := make(chan struct{})
	results := make(chan error, 8)
	var saves atomic.Int32
	for range cap(results) {
		a := begin(t, m, t.Context())
		go func() {
			<-start
			defer a.Finish()
			err := verify(a, d.Code)
			if err == nil {
				err = a.Commit(func(context.Context) error { saves.Add(1); return nil })
			}
			results <- err
		}()
	}
	close(start)
	successes := 0
	for range cap(results) {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 || saves.Load() != 1 {
		t.Fatalf("successes %d, saves %d", successes, saves.Load())
	}
	wantErr(t, m.Revoke(t.Context(), d.Generation), ErrRedeemed)
}

func TestStaleCallbacks(t *testing.T) {
	for _, action := range []string{"revoke", "issue", "close"} {
		t.Run(action, func(t *testing.T) {
			m := NewManager(Config{})
			defer m.Close()
			d := issue(t, m)
			late := begin(t, m, t.Context())
			a := begin(t, m, t.Context())
			if err := verify(a, d.Code); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "revoke":
				wantErr(t, m.Revoke(t.Context(), d.Generation), nil)
				issue(t, m)
			case "issue":
				issue(t, m)
			case "close":
				m.Close()
				_, err := m.Issue()
				wantErr(t, err, cpace.ReasonUnavailable)
			}
			if a.Context().Err() == nil || late.Context().Err() == nil {
				t.Fatal("stale handle not canceled")
			}
			before := m.Status()
			_, _, err := late.Respond(testBinding, [32]byte{})
			wantErr(t, err, cpace.ReasonUnavailable)
			wantErr(t, a.Commit(nil), cpace.ReasonUnavailable)
			late.Finish()
			a.Abort()
			a.Finish()
			wantErr(t, m.Revoke(t.Context(), d.Generation), cpace.ReasonUnavailable)
			if m.Status() != before {
				t.Fatal("stale callback changed state")
			}
		})
	}
}

func TestBlockedSave(t *testing.T) {
	for _, fail := range []bool{false, true} {
		m := NewManager(Config{})
		d := issue(t, m)
		ctx, cancel := context.WithCancel(t.Context())
		late := begin(t, m, ctx)
		a := begin(t, m, ctx)
		if err := verify(a, d.Code); err != nil {
			t.Fatal(err)
		}
		_, _, err := late.Respond(testBinding, [32]byte{})
		wantErr(t, err, cpace.ReasonBusy)
		_, err = m.Begin(ctx)
		wantErr(t, err, cpace.ReasonBusy)
		if m.Status().AttemptsLeft != 4 {
			t.Fatal("busy charged a guess")
		}
		entered := make(chan context.Context)
		release := make(chan struct{})
		done := make(chan error)
		go func() {
			done <- a.Commit(func(ctx context.Context) error {
				entered <- ctx
				<-release
				if fail {
					return errors.New("disk failure")
				}
				return ctx.Err()
			})
		}()
		saveCtx := <-entered
		cancel()
		deadline, ok := saveCtx.Deadline()
		if !ok || time.Until(deadline) > 3*time.Second || saveCtx.Err() != nil {
			t.Fatal("invalid save context")
		}
		a.Finish()
		a.Abort()
		if !m.Status().Committing {
			t.Fatal("save lost ownership")
		}
		_, err = m.Display(d.Generation)
		wantErr(t, err, cpace.ReasonUnavailable)
		_, err = m.Begin(t.Context())
		wantErr(t, err, cpace.ReasonBusy)
		_, err = m.Issue()
		wantErr(t, err, cpace.ReasonBusy)
		wantErr(t, m.Revoke(t.Context(), d.Generation), ErrAlreadyRedeeming)
		wantErr(t, a.Commit(nil), cpace.ReasonUnavailable)
		close(release)
		if fail {
			wantErr(t, <-done, cpace.ReasonStoreFailed)
			_, err = m.Begin(t.Context())
			wantErr(t, err, cpace.ReasonStoreFailed)
		} else {
			wantErr(t, <-done, nil)
			wantErr(t, m.Revoke(t.Context(), d.Generation), ErrRedeemed)
		}
		fresh := issue(t, m)
		a.Finish()
		if m.Status() != fresh.Status {
			t.Fatal("late finish changed replacement")
		}
		m.Close()
	}
}

func TestCloseWaitsForSaveDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(Config{})
		d := issue(t, m)
		a := begin(t, m, t.Context())
		if err := verify(a, d.Code); err != nil {
			t.Fatal(err)
		}
		entered := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- a.Commit(func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() })
		}()
		<-entered
		closed := make(chan struct{})
		go func() { m.Close(); close(closed) }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close returned during save")
		default:
		}
		_, err := m.Begin(t.Context())
		wantErr(t, err, cpace.ReasonUnavailable)
		time.Sleep(3 * time.Second)
		<-closed
		wantErr(t, <-done, cpace.ReasonStoreFailed)
	})
}

func TestHandshakeTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := NewManager(Config{})
		defer m.Close()
		d := issue(t, m)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		a := begin(t, m, ctx)
		if err := verify(a, d.Code); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		b := begin(t, m, t.Context())
		defer b.Finish()
		if m.Status().AttemptsLeft != 4 {
			t.Fatal("timeout refunded guess")
		}
		wantErr(t, a.Commit(nil), context.DeadlineExceeded)
	})
}

// testBinding is non-zero because cpace rejects zero exporters and fingerprints.
var testBinding = cpace.Binding{Exporter: [32]byte{2}, ClientFP: [32]byte{3}, OperatorFP: [32]byte{4}}
