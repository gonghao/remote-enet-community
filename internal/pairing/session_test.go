package pairing

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
)

func TestSessionResponderSaveOwnership(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := NewSessionResponder("12345678", 2)
		a, err := r.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		b := testBinding
		b.Capability, b.CenterID, b.SessionID[0], b.HubFP[0] = proto.PairCenterCapability, "center", 1, 2
		if _, _, err := a.Respond(b, [64]byte{}); err != nil {
			t.Fatal(err)
		}
		entered, release := make(chan struct{}), make(chan struct{})
		result := make(chan error, 1)
		go func() {
			result <- a.Commit(func(ctx context.Context) error {
				close(entered)
				<-release
				return ctx.Err()
			})
		}()
		<-entered
		a.Finish()
		if _, err := r.Begin(t.Context()); err == nil || r.AttemptsLeft() != 1 {
			t.Fatal("in-flight save lost ownership")
		}
		if err := a.Commit(nil); err == nil {
			t.Fatal("duplicate commit succeeded")
		}
		closed := make(chan struct{})
		go func() { r.Close(); close(closed) }()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("close returned before pin write")
		default:
		}
		close(release)
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		<-closed
		if _, err := r.Begin(t.Context()); !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	})
}

func TestSessionResponderCloseCancelsAttempt(t *testing.T) {
	r := NewSessionResponder("12345678", 1)
	a, err := r.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	if a.Context().Err() == nil {
		t.Fatal("attempt still live")
	}
	if _, _, err := a.Respond(cpace.Binding{}, [64]byte{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := a.Commit(nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	a.Finish()
}
