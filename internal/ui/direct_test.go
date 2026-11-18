package ui

import (
	"net/netip"
	"testing"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/settings"
)

func TestDirectActionsUseIndependentCompletion(t *testing.T) {
	enables, disables := 0, 0
	v := New(operatorConfig(), Actions{ConnectMGU: func() { enables++ }, DisconnectMGU: func() { disables++ }})
	s := State{Running: true, Error: "An earlier operation failed.", Runtime: app.Snapshot{Phase: app.PhaseConnected, Generation: 1}}
	v.SetState(s)
	tt := ui.NewTester(v.Render, 1080, 760)
	click(t, tt, "Enable MGU direct link")
	v.SetState(s)
	v.Complete(ActionTest)
	tt.Frame()
	click(t, tt, "Enable MGU direct link")
	if enables != 1 {
		t.Fatal("old errors or unrelated completion reopened direct setup")
	}
	click(t, tt, "Disable MGU direct link")
	click(t, tt, "Disable MGU direct link")
	if disables != 1 {
		t.Fatal("direct stop did not cancel setup or admitted duplicates")
	}
	v.Complete(ActionConnectMGU)
	v.Complete(ActionDisconnectMGU)
	s.Runtime.Direct = app.DirectState{Phase: app.DirectReady, Address: netip.MustParseAddr("169.254.9.3")}
	v.SetState(s)
	tt.Frame()
	if !tt.HasText("169.254.9.3") || !tt.HasText("Ready") {
		t.Fatal("ready link did not display its head-unit address")
	}
}

func TestClientDoesNotExposeDirectActions(t *testing.T) {
	v := New(settings.Default(), Actions{ConnectMGU: func() { t.Error("Client admitted Operator action") }, DisconnectMGU: func() { t.Error("Client admitted Operator action") }})
	tt := ui.NewTester(v.Render, 820, 640)
	for _, label := range []string{"Enable MGU direct link", "Disable MGU direct link"} {
		if _, found := tt.Find(label); found {
			t.Fatal("Client exposed a direct-access control")
		}
	}
}
