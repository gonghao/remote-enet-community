package exclusive

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResolverRetriesDelayedRow(t *testing.T) {
	calls := 0
	want := Identity{PID: 12, Created: 34}
	got, err := resolveOwner(t.Context(), func(context.Context) (Identity, error) {
		calls++
		if calls < 3 {
			return Identity{}, ErrUnknownOwner
		}
		return want, nil
	})
	if err != nil || got != want || calls != 3 {
		t.Fatalf("delayed row: owner=%+v calls=%d err=%v", got, calls, err)
	}
}

func TestResolverQueryFailureDoesNotRetry(t *testing.T) {
	queryErr := errors.New("TCP table access denied")
	calls := 0
	got, err := resolveOwner(t.Context(), func(context.Context) (Identity, error) {
		calls++
		return Identity{}, queryErr
	})
	if got != (Identity{}) || !errors.Is(err, queryErr) || calls != 1 {
		t.Fatalf("query failure: owner=%+v calls=%d err=%v", got, calls, err)
	}
}

func TestResolverMissingRowBudgetAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
		if canceled {
			cancel()
		}
		calls := 0
		got, err := resolveOwner(ctx, func(context.Context) (Identity, error) {
			calls++
			return Identity{}, ErrUnknownOwner
		})
		wantErr := ctx.Err()
		cancel()
		if got != (Identity{}) || !errors.Is(err, ErrUnknownOwner) || !errors.Is(err, wantErr) || (canceled && calls != 0) {
			t.Fatalf("missing row: owner=%+v calls=%d err=%v", got, calls, err)
		}
	}
	// The native resolver itself has a budget even outside a Gate.
	calls := 0
	_, err := resolveOwner(t.Context(), func(ctx context.Context) (Identity, error) {
		calls++
		if _, ok := ctx.Deadline(); !ok {
			t.Error("lookup without deadline")
		}
		return Identity{}, ErrUnknownOwner
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls > 10 {
		t.Fatalf("missing row rebuilt the TCP table too often: %d", calls)
	}
}

func TestResolverDiscardsIdentityAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	got, err := resolveOwner(ctx, func(context.Context) (Identity, error) {
		cancel()
		return Identity{PID: 1, Created: 2}, nil
	})
	if got != (Identity{}) || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled lookup returned owner: %+v, %v", got, err)
	}
}
