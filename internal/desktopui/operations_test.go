package desktopui

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/desktop"
	"remote-enet/internal/settings"
	renetui "remote-enet/internal/ui"
)

func testShell(t *testing.T) (*shell, chan func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	dir := t.TempDir()
	controller, err := desktop.New(ctx, desktop.Options{ConfigPath: filepath.Join(dir, "gui.json"), StateDir: filepath.Join(dir, "state")})
	if err != nil {
		t.Fatal(err)
	}
	product, err := productForRole(settings.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	s := newShell(ctx, cancel, controller, product)
	s.state = controller.Snapshot()
	s.view = renetui.New(s.state.Config, renetui.Actions{})
	posted := make(chan func(), 16)
	s.post = func(fn func()) { posted <- fn }
	s.invalidate = func() {}
	t.Cleanup(func() {
		cancel()
		if err := controller.Close(context.Background()); err != nil {
			t.Error(err)
		}
		s.workers.Wait()
	})
	return s, posted
}

func waitPosted(t *testing.T, posted <-chan func()) {
	t.Helper()
	select {
	case fn := <-posted:
		fn()
	case <-time.After(2 * time.Second):
		t.Fatal("UI completion was not posted")
	}
}

func TestActionAdmissionReservesAndCoalescesStop(t *testing.T) {
	s, posted := testShell(t)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	blocked := func(ctx context.Context) error {
		started <- struct{}{}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}
	for _, kind := range []renetui.Action{renetui.ActionRefresh, renetui.ActionSave, renetui.ActionInvite, renetui.ActionConnect} {
		s.submit(kind, blocked)
	}
	for range 4 {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("ordinary request did not start")
		}
	}
	s.submit(renetui.ActionListen, func(context.Context) error {
		t.Error("full ordinary action budget admitted another request")
		return nil
	})
	if s.commandErr == "" {
		t.Fatal("quota failure did not produce feedback")
	}
	stopping := make(chan struct{})
	s.submit(renetui.ActionStop, func(context.Context) error { close(stopping); close(release); return nil })
	s.submit(renetui.ActionStop, func(context.Context) error { t.Error("duplicate pending Stop was not coalesced"); return nil })
	select {
	case <-stopping:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop was blocked by the ordinary action quota")
	}
	s.workers.Wait()
	for range 5 {
		waitPosted(t, posted)
	}
	if len(s.requests) != 0 || s.stopPending {
		t.Fatal("completed requests retained admission state")
	}
}

func TestDirectDisableHasIndependentReservedAdmission(t *testing.T) {
	s, posted := testShell(t)
	release := make(chan struct{})
	defer close(release)
	started := make(chan struct{}, 4)
	for _, action := range []renetui.Action{renetui.ActionRefresh, renetui.ActionSave, renetui.ActionTest, renetui.ActionConnectMGU} {
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
			t.Fatal("ordinary action did not start")
		}
	}
	disabling := make(chan struct{})
	s.submit(renetui.ActionDisconnectMGU, func(ctx context.Context) error {
		close(disabling)
		<-ctx.Done()
		return ctx.Err()
	})
	s.submit(renetui.ActionDisconnectMGU, func(context.Context) error {
		t.Error("duplicate pending disable was admitted")
		return nil
	})
	select {
	case <-disabling:
	case <-time.After(time.Second):
		t.Fatal("full ordinary queue blocked direct disable")
	}
	s.submit(renetui.ActionStop, func(context.Context) error { s.cancel(); return nil })
	s.workers.Wait()
	for range 6 {
		waitPosted(t, posted)
	}
	if s.directStopPending || s.stopPending || len(s.requests) != 0 {
		t.Fatal("completed reserved actions retained admission")
	}
}

func TestActionCompletionUsesProductErrorAndFreshSnapshot(t *testing.T) {
	s, posted := testShell(t)
	raw := errors.New("private protocol failure renet1:PRIVATE /private/data")
	s.state.Notice = "an obsolete snapshot"
	s.submit(renetui.ActionRefresh, func(context.Context) error { return raw })
	waitPosted(t, posted)
	if s.state.Notice != s.controller.Snapshot().Notice || strings.Contains(s.commandErr, "PRIVATE") || strings.Contains(s.commandErr, "private") {
		t.Fatal("completion displayed obsolete state or raw error data")
	}
	s.state.Error, s.state.Notice = "Settings could not be saved.", ""
	s.commandErr = operationMessage(raw, s.state.Error)
	s.applyState()
	tt := ui.NewTester(s.view.Render, 1080, 780)
	if !tt.HasText("Settings could not be saved.") || tt.HasText("private protocol failure") {
		t.Fatal("product error was replaced by internal diagnostics")
	}
}

func TestLateRequestErrorCannotReplaceNewerResult(t *testing.T) {
	s, posted := testShell(t)
	release := make(chan struct{})
	s.submit(renetui.ActionRefresh, func(ctx context.Context) error {
		select {
		case <-release:
			return errors.New("obsolete request failed")
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	s.submit(renetui.ActionRefresh, func(context.Context) error { return nil })
	waitPosted(t, posted)
	close(release)
	waitPosted(t, posted)
	if s.commandErr != "" {
		t.Fatal("a superseded request replaced a successful newer result")
	}
}

func TestSystemNoticeSurvivesSubscriptionAndActionUpdates(t *testing.T) {
	s, _ := testShell(t)
	s.systemNotice = "Tray is unavailable. Closing the window will quit the application."
	s.applyState()
	tt := ui.NewTester(s.view.Render, 1080, 780)
	s.state = s.controller.Snapshot()
	s.commandErr = ""
	s.applyState()
	tt.Frame()
	if !tt.HasText(s.systemNotice) {
		t.Fatal("a subscription update removed the persistent system notice")
	}
	s.commandErr = "Another action is still finishing. Please try again."
	s.applyState()
	tt.Frame()
	if !tt.HasText(s.systemNotice) || !tt.HasText(s.commandErr) {
		t.Fatal("action feedback hid the persistent system notice")
	}
}

func TestRecoveryButtonSubmitsControllerAction(t *testing.T) {
	s, posted := testShell(t)
	var actions renetui.Actions
	s.centerActions(&actions)
	item := centerflow.SessionCandidate{ID: center.SessionID{1}, ShortID: "AQAAAA", RedeemedAt: time.Now()}
	actions.CenterSessionCandidates = func() []centerflow.SessionCandidate { return []centerflow.SessionCandidate{item} }
	s.view = renetui.New(s.state.Config, actions)
	tt := ui.NewTester(s.view.Render, 440, 700)
	label := item.ShortID + " · " + item.RedeemedAt.Local().Format(time.DateTime)
	if button, ok := tt.AccessByRole(ui.RoleButton, label); !ok || button.Disabled {
		t.Fatal("resume button unavailable")
	}
	if err := s.controller.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := tt.Click(label); err != nil {
		t.Fatal(err)
	}
	waitPosted(t, posted)
	if s.commandErr != operationMessage(desktop.ErrClosed, "") || s.controller.Snapshot().Running {
		t.Fatal("resume action did not report the closed controller", s.commandErr)
	}
}
