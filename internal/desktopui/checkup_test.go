package desktopui

import (
	"context"
	"testing"
	"time"

	renetui "remote-enet/internal/ui"
)

func TestCheckCancellationHasIndependentAdmission(t *testing.T) {
	s, posted := testShell(t)
	release := make(chan struct{})
	started := make(chan struct{}, 4)
	for _, action := range []renetui.Action{renetui.ActionRefresh, renetui.ActionSave, renetui.ActionInvite, renetui.ActionCheckConnection} {
		s.submit(action, func(ctx context.Context) error {
			started <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		})
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("ordinary operation did not start")
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	s.checksCancel = cancel
	cancelStarted := make(chan struct{})
	if !s.submit(renetui.ActionCancelChecks, func(context.Context) error { close(cancelStarted); <-release; return nil }) {
		t.Fatal("full queue blocked cancel checks")
	}
	if s.submit(renetui.ActionCancelChecks, func(context.Context) error { t.Error("duplicate cancellation admitted"); return nil }) {
		t.Fatal("duplicate cancellation not coalesced")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("pending request context was not canceled before controller admission")
	}
	select {
	case <-cancelStarted:
	case <-time.After(time.Second):
		t.Fatal("cancel worker did not run")
	}
	if !s.submit(renetui.ActionStop, func(context.Context) error { close(release); return nil }) {
		t.Fatal("cancel slot blocked global Stop")
	}
	s.workers.Wait()
	for range 6 {
		waitPosted(t, posted)
	}
	if s.checksCancelPending || s.stopPending || len(s.requests) != 0 {
		t.Fatal("admission state leaked")
	}
}
