package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

func openCapture(t *testing.T, tt *ui.Tester) {
	t.Helper()
	click(t, tt, "Log")
	click(t, tt, "Open HSFZ capture")
	tt.Frame()
}

func TestCaptureCompletionIsolation(t *testing.T) {
	starts, stops := 0, 0
	v := New(operatorConfig(), Actions{StartCapture: func() { starts++ }, StopCapture: func() { stops++ }})
	state := State{Running: true, Error: "Old failure"}
	v.SetState(state)
	tt := ui.NewTester(v.Render, 1080, 760)
	openCapture(t, tt)
	click(t, tt, "Start HSFZ capture")
	for _, a := range []Action{ActionRefresh, ActionStop, ActionStopCapture} {
		v.SetState(state)
		v.Complete(a)
		tt.Frame()
		click(t, tt, "Start HSFZ capture")
	}
	if starts != 1 {
		t.Fatal("unrelated completion or old state unlocked start")
	}
	click(t, tt, "Stop HSFZ capture")
	if stops != 1 {
		t.Fatal("pending start could not be cancelled")
	}
	v.Complete(ActionStartCapture)
	tt.Frame()
	click(t, tt, "Stop HSFZ capture")
	if stops != 1 {
		t.Fatal("start completion unlocked stop")
	}
	state.Capture.Active = true
	v.SetState(state)
	tt.Frame()
	click(t, tt, "Stop HSFZ capture")
	if stops != 1 {
		t.Fatal("snapshot unlocked stop")
	}
	v.Complete(ActionStopCapture)
	tt.Frame()
	click(t, tt, "Stop HSFZ capture")
	if stops != 2 {
		t.Fatal("matching completion did not unlock stop")
	}
	v.Complete(ActionStopCapture)
	state.Capture.Active = false
	v.SetState(state)
	tt.Frame()
	click(t, tt, "Start HSFZ capture")
	if starts != 2 {
		t.Fatal("matching completion did not unlock start")
	}
}

func TestCaptureFrameInteractionAndBounds(t *testing.T) {
	var copied []string
	v := New(operatorConfig(), Actions{Copy: func(s string) { copied = append(copied, s) }})
	state := State{}
	for i := range 250 {
		state.Capture.Recent = append(state.Capture.Recent, hsfzcapture.Frame{Sequence: uint64(i), Connection: uint64(i%2 + 1), Generation: 3, Direction: hsfzcapture.ToVehicle, Control: 1, BodyLength: 9999, HasAddresses: true, Source: 0xF4, Destination: 0x10, Status: "partial", PrefixHex: strings.Repeat("ab", 256), PrefixTruncated: true})
	}
	v.SetState(state)
	tt := ui.NewTester(v.Render, 1080, 1000)
	openCapture(t, tt)
	if _, ok := tt.Find("Select capture frame 49"); ok {
		t.Fatal("old frames exceeded list bound")
	}
	count := 0
	for _, text := range tt.Texts() {
		if strings.HasPrefix(text, "Frame ") && strings.Contains(text, " · conn ") {
			count++
		}
	}
	if count != 200 {
		t.Fatalf("rendered %d frames", count)
	}
	scrollTo(t, tt, "Select capture frame 249", 1080, 1000)
	click(t, tt, "Select capture frame 249")
	if len(copied) != 0 {
		t.Fatal("selection copied without explicit request")
	}
	scrollTo(t, tt, "Copy shown hex", 1080, 1000)
	click(t, tt, "Copy shown hex")
	if len(copied) != 1 || copied[0] != strings.Repeat("ab", 256) {
		t.Fatal("copy did not preserve shown prefix")
	}
	if !tt.HasText("Source 0xF4 · destination 0x10") || !tt.HasText("Frame status: partial") || !tt.HasText("Prefix truncated · this is not a complete large frame.") {
		t.Fatal("missing frame details")
	}
	scrollTo(t, tt, "Capture connection filter", 1080, 1000)
	input(t, tt, "Capture connection filter", "1")
	if _, ok := tt.Find("Select capture frame 249"); ok {
		t.Fatal("connection filter did not hide other connection")
	}
	if _, ok := tt.Find("Copy shown hex"); ok {
		t.Fatal("filtered selection remained copyable")
	}
	click(t, tt, "Capture direction filter")
	click(t, tt, string(hsfzcapture.FromVehicle))
	if !tt.HasText("No matching recent frames.") {
		t.Fatal("direction filter was not applied")
	}
	if len(copied) != 1 {
		t.Fatal("filtering copied data")
	}
}

func TestCaptureRoleAndAdmission(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			cfg := operatorConfig()
			cfg.Role = role
			calls := 0
			v := New(cfg, Actions{StartCapture: func() { calls++ }, StopCapture: func() { calls++ }})
			tt := ui.NewTester(v.Render, 420, 420)
			click(t, tt, "Log")
			if role == settings.RoleClient {
				if _, ok := tt.Find("Open HSFZ capture"); ok {
					t.Fatal("client exposed capture entry")
				}
				v.page = capturePage
				v.SetState(State{Running: true, Capture: hsfzcapture.Snapshot{Active: true}})
				tt.Frame()
				for _, label := range []string{"Start HSFZ capture", "Stop HSFZ capture", "Copy shown hex"} {
					if _, ok := tt.Find(label); ok {
						t.Fatal("client exposed capture action")
					}
				}
				if v.canStartCapture() || v.canStopCapture() {
					t.Fatal("client admitted capture")
				}
				return
			}
			click(t, tt, "Open HSFZ capture")
			tt.Frame()
			for _, state := range []State{{}, {Running: true, Stopping: true}, {Running: true, Capture: hsfzcapture.Snapshot{Active: true}}, {Running: true, Capture: hsfzcapture.Snapshot{Stopping: true}}} {
				v.SetState(state)
				tt.Frame()
				click(t, tt, "Start HSFZ capture")
			}
			if calls != 0 {
				t.Fatal("invalid runtime admitted start")
			}
			for _, status := range []string{"partial", "gap", "oversized"} {
				v.SetState(State{Capture: hsfzcapture.Snapshot{StartedAt: time.Now(), Error: "file_limit", Incomplete: true, Path: strings.Repeat("long\n", 500), Recent: []hsfzcapture.Frame{{Sequence: 1, Status: status, PrefixHex: strings.Repeat("ff", 10000)}}}})
				tt.Frame()
				scrollTo(t, tt, "Select capture frame 1", 420, 420)
				click(t, tt, "Select capture frame 1")
				if !tt.HasText("Frame status: " + status) {
					t.Fatal(fmt.Sprintf("missing %s", status))
				}
				r, ok := tt.Find("Back to session")
				if !ok || r.Y < 0 || r.Y+r.H > 420 {
					t.Fatal("Back is unreachable")
				}
			}
			click(t, tt, "Back to session")
		})
	}
}

func TestCaptureSnapshotOwnershipAndNewRecordingSelection(t *testing.T) {
	var copied string
	v := New(operatorConfig(), Actions{Copy: func(s string) { copied = s }})
	state := State{Capture: hsfzcapture.Snapshot{StartedAt: time.Unix(1, 0), Recent: []hsfzcapture.Frame{{Sequence: 1, PrefixHex: strings.Repeat("cd", 10000), Status: "oversized"}}}}
	v.SetState(state)
	state.Capture.Recent[0].PrefixHex = "changed"
	tt := ui.NewTester(v.Render, 1080, 1000)
	openCapture(t, tt)
	click(t, tt, "Select capture frame 1")
	tt.Frame()
	click(t, tt, "Copy shown hex")
	if copied != strings.Repeat("cd", 256) {
		t.Fatal("snapshot was aliased or long prefix was not bounded")
	}
	state.Capture.StartedAt = time.Unix(2, 0)
	v.SetState(state)
	tt.Frame()
	if _, ok := tt.Find("Copy shown hex"); ok {
		t.Fatal("new recording reused the previous recording's selection")
	}
	if !tt.HasText("Select a recent frame to inspect its prefix.") {
		t.Fatal("new recording did not clear selection")
	}
}
