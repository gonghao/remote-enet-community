package ui

import (
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/settings"
)

func TestUnrelatedErrorsDoNotCompletePendingActions(t *testing.T) {
	for _, action := range []Action{ActionSave, ActionTest, ActionInvite} {
		for _, message := range []string{"Old operation failed.", "Adapter refresh failed."} {
			t.Run(actionName(action)+"/"+message, func(t *testing.T) {
				calls := 0
				v := New(operatorConfig(), Actions{
					Save: func(settings.Config) { calls++ }, TestLink: func() { calls++ }, IssueInvite: func() { calls++ },
				})
				state := State{Error: "Old operation failed."}
				if action == ActionTest {
					state.Running, state.Runtime.Phase = true, app.PhaseConnected
				}
				v.SetState(state)
				tt := ui.NewTester(v.Render, 1080, 1500)
				if action == ActionSave {
					openSettings(t, tt)
					click(t, tt, "Allow peer connection tests")
				}
				label := actionName(action)
				click(t, tt, label)
				if calls != 1 {
					t.Fatal("initial request was not dispatched")
				}
				baseline := v.saved
				draft, err := v.config()
				if err != nil {
					t.Fatal(err)
				}
				state.Error = message
				v.SetConfig(baseline)
				v.SetState(state)
				v.Complete(ActionRefresh)
				tt.Frame()
				click(t, tt, label)
				if calls != 1 {
					t.Fatal("an old or unrelated error/completion admitted a duplicate request")
				}
				v.Complete(action)
				current, err := v.config()
				if err != nil || current != draft || v.saved != baseline {
					t.Fatal("completion changed the form or saved configuration")
				}
				tt.Frame()
				click(t, tt, label)
				if calls != 2 {
					t.Fatal("the matching completion did not allow retry")
				}
			})
		}
	}
}

func TestStartErrorsRequireMatchingCompletion(t *testing.T) {
	for _, action := range []Action{ActionConnect, ActionListen} {
		t.Run(actionName(action), func(t *testing.T) {
			calls := 0
			cfg := operatorConfig()
			if action == ActionConnect {
				cfg.Role = settings.RoleClient
			}
			v := New(cfg, Actions{Connect: func(string) { calls++ }, Listen: func() { calls++ }})
			state := State{Error: "Old operation failed."}
			v.SetState(state)
			tt := ui.NewTester(v.Render, 1080, 1500)
			if action == ActionConnect {
				input(t, tt, "Invite or address:port and code", "test-only-code")
			}
			label := actionName(action)
			click(t, tt, label)
			for _, message := range []string{state.Error, "Adapter refresh failed."} {
				state.Error = message
				v.SetConfig(cfg)
				v.SetState(state)
				v.Complete(ActionRefresh)
				other := ActionListen
				if action == ActionListen {
					other = ActionConnect
				}
				v.Complete(other)
				tt.Frame()
				click(t, tt, label)
				if calls != 1 {
					t.Fatal("an unrelated error or start completion admitted a duplicate start")
				}
			}
			v.Complete(action)
			tt.Frame()
			if action == ActionConnect {
				click(t, tt, label)
				if calls != 1 || v.invitation != "" {
					t.Fatal("the previous secret was retained after failed admission")
				}
				input(t, tt, "Invite or address:port and code", "test-only-code")
			}
			click(t, tt, label)
			if calls != 2 {
				t.Fatal("failed start was not retryable after its completion")
			}
		})
	}
}

func TestRefreshCompletionDoesNotUnlockStop(t *testing.T) {
	refreshes, stops := 0, 0
	v := New(settings.Default(), Actions{Refresh: func() { refreshes++ }, Stop: func() { stops++ }})
	tt := ui.NewTester(v.Render, 1080, 1500)
	click(t, tt, "Refresh adapters")
	click(t, tt, "Refresh adapters")
	if refreshes != 1 {
		t.Fatal("refresh admitted duplicate requests")
	}
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseConnected}})
	tt.Frame()
	click(t, tt, "Stop runtime")
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseConnected}, Error: "Adapter refresh failed."})
	v.Complete(ActionRefresh)
	tt.Frame()
	click(t, tt, "Stop runtime")
	if stops != 1 {
		t.Fatal("refresh completion or error admitted a duplicate stop")
	}
	v.Complete(ActionStop)
	tt.Frame()
	click(t, tt, "Stop runtime")
	if stops != 2 {
		t.Fatal("stop could not be retried after its completion")
	}
}

func actionName(action Action) string {
	switch action {
	case ActionSave:
		return "Save settings"
	case ActionConnect:
		return "Connect client"
	case ActionListen:
		return "Start operator"
	case ActionInvite:
		return "Generate invitation"
	case ActionTest:
		return "Test connection"
	default:
		panic("unsupported test action")
	}
}

func TestClearingPreviousInvitationDoesNotCompleteGeneration(t *testing.T) {
	calls := 0
	v := New(operatorConfig(), Actions{IssueInvite: func() { calls++ }})
	v.SetState(State{Invitation: "renet1:OLD"})
	tt := ui.NewTester(v.Render, 1080, 1300)
	click(t, tt, "Generate invitation")
	v.SetState(State{})
	tt.Frame()
	click(t, tt, "Generate invitation")
	if calls != 1 {
		t.Fatal("clearing a previous invitation admitted duplicate generation")
	}
	v.Complete(ActionInvite)
	tt.Frame()
	click(t, tt, "Generate invitation")
	if calls != 2 {
		t.Fatal("an explicit completion did not allow another invitation")
	}
}
