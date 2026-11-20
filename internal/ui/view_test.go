package ui

import (
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/observe"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

func click(t *testing.T, tt *ui.Tester, label string) {
	t.Helper()
	if err := tt.Click(label); err != nil {
		t.Fatal(err)
	}
}

func input(t *testing.T, tt *ui.Tester, label, value string) {
	t.Helper()
	click(t, tt, label)
	tt.Command("selectAll")
	tt.Type(value)
}

func openSettings(t *testing.T, tt *ui.Tester) {
	t.Helper()
	if _, ok := tt.Find("Save settings"); !ok {
		click(t, tt, "Settings")
	}
}

func openSession(t *testing.T, tt *ui.Tester) {
	t.Helper()
	if _, ok := tt.Find("Back to session"); ok {
		click(t, tt, "Back to session")
	}
}

func scrollTo(t *testing.T, tt *ui.Tester, label string, width, height float32) {
	t.Helper()
	for range 24 {
		r, ok := tt.Find(label)
		if !ok {
			t.Fatalf("missing control %q", label)
		}
		if r.Y >= 60 && r.Y+r.H < height-8 && r.H > 0 {
			return
		}
		dy := float32(120)
		if r.Y < 60 {
			dy = -120
		}
		tt.Scroll(width-12, height/2, 0, dy)
	}
	t.Fatalf("control %q did not scroll into view", label)
}

func operatorConfig() settings.Config {
	cfg := settings.Default()
	cfg.Role = settings.RoleOperator
	cfg.Operator.Endpoint, cfg.Operator.Bind = "localhost:47000", "127.0.0.2"
	return cfg
}

func TestClientInputAndLifecycle(t *testing.T) {
	var connections []string
	stops, refreshes, saves, tests, quits := 0, 0, 0, 0, 0
	v := New(settings.Default(), Actions{
		Connect: func(code string) { connections = append(connections, code) },
		Stop:    func() { stops++ }, Refresh: func() { refreshes++ },
		Save:     func(settings.Config) { saves++ },
		TestLink: func() { tests++ }, Quit: func() { quits++ },
	})
	tt := ui.NewTester(v.Render, 1080, 1200)
	click(t, tt, "Connect client")
	click(t, tt, "Stop runtime")
	openSettings(t, tt)
	click(t, tt, "Test connection")
	if len(connections) != 0 || stops != 0 || tests != 0 {
		t.Fatal("idle controls admitted an unavailable action")
	}
	openSession(t, tt)
	click(t, tt, "Refresh adapters")
	v.Complete(ActionRefresh)
	tt.Frame()
	click(t, tt, "Invite or address:port and code")
	if !tt.Focused("Invite or address:port and code") {
		t.Fatal("invitation label did not focus the native editor")
	}
	tt.SetClipboard("  test-only-code  ")
	tt.Command("paste")
	click(t, tt, "Connect client")
	click(t, tt, "Connect client")
	if _, ok := tt.Find("Workspace role"); ok {
		t.Fatal("client exposed a role selector")
	}
	if len(connections) != 1 || connections[0] != "test-only-code" || v.draft.Role != settings.RoleClient {
		t.Fatalf("start did not guard repeat requests: calls=%d role=%s", len(connections), v.draft.Role)
	}
	s := State{
		Running: true, Runtime: app.Snapshot{Phase: app.PhaseWaitingNIC},
		NICs: []carside.NIC{{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")}},
	}
	v.SetState(s)
	tt.Frame()
	if !tt.HasText("Waiting for adapter") || v.invitation != "" {
		t.Fatal("running state did not update stage or clear transient invitation")
	}
	input(t, tt, "Invite or address:port and code", "blocked")
	click(t, tt, "Refresh adapters")
	click(t, tt, "Adapter selection")
	if _, found := tt.Find("Ethernet · 169.254.10.1"); found {
		t.Fatal("running client opened the adapter dropdown")
	}
	openSettings(t, tt)
	click(t, tt, "Test connection")
	if v.invitation != "" || refreshes != 1 || saves != 0 || tests != 0 {
		t.Fatal("running form or disconnected test accepted input")
	}
	s.Runtime.Phase = app.PhaseConnected
	v.SetState(s)
	tt.Frame()
	click(t, tt, "Test connection")
	click(t, tt, "Test connection")
	if tests != 1 {
		t.Fatal("test action was not admitted exactly once")
	}
	s.Testing = true
	v.SetState(s)
	tt.Frame()
	click(t, tt, "Test connection")
	openSession(t, tt)
	click(t, tt, "Stop runtime")
	click(t, tt, "Stop runtime")
	if tests != 1 || stops != 1 {
		t.Fatal("active test or stopping controls admitted duplicate work")
	}
	s.Stopping = true
	v.SetState(s)
	tt.Frame()
	if !tt.HasText("Stopping") {
		t.Fatal("missing stopping phase")
	}
	v.SetState(State{Runtime: app.Snapshot{Phase: app.PhaseStopped}})
	tt.Frame()
	if v.draft.Role != settings.RoleClient || tt.HasText("Workspace role") || tt.HasText("Start operator") {
		t.Fatal("stopping changed the fixed client role")
	}
	click(t, tt, "Quit Remote ENET")
	if quits != 1 {
		t.Fatal("quit callback not dispatched")
	}
}

func TestSaveAcknowledgementAndPeerMerge(t *testing.T) {
	cfg := operatorConfig()
	var requests []settings.Config
	starts := 0
	v := New(cfg, Actions{Save: func(c settings.Config) { requests = append(requests, c) }, Listen: func() { starts++ }})
	tt := ui.NewTester(v.Render, 1080, 1400)
	openSettings(t, tt)
	input(t, tt, "Public endpoint", "new.example:47000")
	openSession(t, tt)
	click(t, tt, "Start operator")
	openSettings(t, tt)
	click(t, tt, "Save settings")
	click(t, tt, "Save settings")
	if len(requests) != 1 || starts != 0 {
		t.Fatal("unsaved configuration started or save was dispatched twice")
	}
	v.SetConfig(cfg)
	v.SetState(State{})
	tt.Frame()
	openSession(t, tt)
	click(t, tt, "Start operator")
	openSettings(t, tt)
	if v.submitted == nil || starts != 0 || !tt.HasText("Saving settings") {
		t.Fatal("an old configuration acknowledged a new save")
	}
	input(t, tt, "Presentation IPv4", "127.0.0.3")
	v.SetConfig(requests[0])
	v.SetState(State{Notice: "Settings saved."})
	tt.Frame()
	if v.submitted != nil || !v.dirty() || v.draft.Operator.Bind != "127.0.0.3" {
		t.Fatal("save acknowledgement discarded newer edits")
	}
	click(t, tt, "Save settings")
	v.SetConfig(requests[0])
	v.SetState(State{Error: "Settings could not be written."})
	v.Complete(ActionSave)
	tt.Frame()
	if !tt.HasText("Settings could not be written.") || v.submitted != nil || !v.dirty() {
		t.Fatal("failed save lost form edits or remained pending")
	}
	openSession(t, tt)
	click(t, tt, "Start operator")
	openSettings(t, tt)
	click(t, tt, "Save settings")
	if len(requests) != 3 || starts != 0 {
		t.Fatal("failed save was not retryable or allowed start")
	}
	v.SetConfig(requests[2])
	v.SetState(State{Notice: "Settings saved."})
	tt.Frame()
	openSession(t, tt)
	click(t, tt, "Start operator")
	if starts != 1 || v.dirty() {
		t.Fatal("confirmed settings did not enable start")
	}

	client := New(settings.Default(), Actions{Save: func(c settings.Config) { requests = append(requests, c) }})
	ct := ui.NewTester(client.Render, 1080, 1200)
	openSettings(t, ct)
	click(t, ct, "Allow peer connection tests")
	paired := settings.Default()
	paired.Client.Peer = (identity.Fingerprint{1}).String()
	client.SetConfig(paired)
	ct.Frame()
	click(t, ct, "Save settings")
	if requests[len(requests)-1].Client.Peer != paired.Client.Peer || client.draft.AllowLinkTest {
		t.Fatal("saving unrelated edits would discard the remembered peer")
	}

	clean := New(settings.Default(), Actions{Connect: func(string) {}})
	clean.SetConfig(paired)
	if clean.draft.Client.Peer != paired.Client.Peer || !clean.canConnect() {
		t.Fatal("clean form did not learn the remembered peer")
	}
}

func TestNICSelectionSurvivesRefresh(t *testing.T) {
	cfg := settings.Default()
	cfg.Client.NICName, cfg.Client.NICAddr = "Ethernet", "169.254.10.1"
	var saved settings.Config
	v := New(cfg, Actions{Save: func(c settings.Config) { saved = c }, Refresh: func() {}})
	s := State{NICs: []carside.NIC{{Name: "USB LAN", Address: netip.MustParseAddr("169.254.20.1")}}}
	v.SetState(s)
	tt := ui.NewTester(v.Render, 1080, 1000)
	if !tt.HasText("Ethernet · 169.254.10.1 (saved selection)") || v.dirty() {
		t.Fatal("missing fixed adapter silently changed the saved choice")
	}
	click(t, tt, "Adapter selection")
	click(t, tt, "USB LAN · 169.254.20.1")
	if saved.Client.NICName != "USB LAN" || saved.Client.NICAddr != "169.254.20.1" {
		t.Fatalf("adapter selection was not saved: %+v", saved.Client)
	}
	v.SetConfig(saved)
	v.SetState(State{})
	tt.Frame()
	if v.draft.Client.NICAddr != "169.254.20.1" {
		t.Fatal("refresh replaced the fixed IP")
	}
	click(t, tt, "Adapter selection")
	click(t, tt, "Automatic")
	if saved.Client.NICName != "" || saved.Client.NICAddr != "" {
		t.Fatal("Automatic did not clear both selectors")
	}
}

func TestClientAdapterSavePreservesSettingsDraft(t *testing.T) {
	for _, editPreference := range []bool{false, true} {
		t.Run(fmt.Sprintf("editPreference=%t", editPreference), func(t *testing.T) {
			cfg := settings.Default()
			var requests []settings.Config
			connects := 0
			v := New(cfg, Actions{
				Save:    func(c settings.Config) { requests = append(requests, c) },
				Connect: func(string) { connects++ },
			})
			state := State{NICs: []carside.NIC{
				{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")},
				{Name: "USB LAN", Address: netip.MustParseAddr("169.254.20.1")},
			}}
			v.SetState(state)
			tt := ui.NewTester(v.Render, clientWidth, clientHeight)
			input(t, tt, "Invite or address:port and code", "test-only-code")
			if editPreference {
				openSettings(t, tt)
				click(t, tt, "Allow peer connection tests")
				openSession(t, tt)
			}

			click(t, tt, "Adapter selection")
			click(t, tt, "Ethernet · 169.254.10.1")
			want := cfg
			want.Client.NICName, want.Client.NICAddr = "Ethernet", "169.254.10.1"
			if len(requests) != 1 || !requests[0].Equal(want) || v.submitted == nil || v.draft.Client != want.Client {
				t.Fatalf("adapter choice did not submit only the NIC fields: %+v", requests)
			}
			v.SetConfig(cfg)
			tt.Frame()
			click(t, tt, "Adapter selection")
			if _, found := tt.Find("USB LAN · 169.254.20.1"); found || v.submitted == nil {
				t.Fatal("the old configuration unlocked the pending adapter selection")
			}
			click(t, tt, "Connect client")
			if connects != 0 || len(requests) != 1 {
				t.Fatal("pending adapter save admitted an extra request")
			}

			v.SetConfig(want)
			v.SetState(state)
			v.Complete(ActionSave)
			tt.Frame()
			wantDraft := want
			wantConnects := 1
			if editPreference {
				wantDraft.AllowLinkTest = !cfg.AllowLinkTest
				wantConnects = 0
			}
			if v.submitted != nil || !v.saved.Equal(want) || !v.draft.Equal(wantDraft) || v.dirty() != editPreference {
				t.Fatal("adapter save acknowledgement did not preserve the remaining edits")
			}
			click(t, tt, "Adapter selection")
			if _, found := tt.Find("USB LAN · 169.254.20.1"); !found {
				t.Fatal("acknowledgement did not re-enable the adapter dropdown")
			}
			tt.Key(0, ui.KeyEscape)
			click(t, tt, "Connect client")
			if connects != wantConnects {
				t.Fatalf("Connect called %d times, want %d", connects, wantConnects)
			}
		})
	}
}

func TestClientAdapterSaveFailureRestoresSelection(t *testing.T) {
	for _, editPreference := range []bool{false, true} {
		t.Run(fmt.Sprintf("editPreference=%t", editPreference), func(t *testing.T) {
			cfg := settings.Default()
			cfg.Client.NICName, cfg.Client.NICAddr = "Ethernet", "169.254.10.1"
			var requests []settings.Config
			v := New(cfg, Actions{
				Save:    func(c settings.Config) { requests = append(requests, c) },
				Connect: func(string) {},
			})
			state := State{NICs: []carside.NIC{
				{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")},
				{Name: "USB LAN", Address: netip.MustParseAddr("169.254.20.1")},
			}}
			v.SetState(state)
			tt := ui.NewTester(v.Render, clientWidth, clientHeight)
			input(t, tt, "Invite or address:port and code", "test-only-code")
			wantDraft := cfg
			if editPreference {
				openSettings(t, tt)
				click(t, tt, "Allow peer connection tests")
				openSession(t, tt)
				wantDraft.AllowLinkTest = !cfg.AllowLinkTest
			}

			click(t, tt, "Adapter selection")
			click(t, tt, "USB LAN · 169.254.20.1")
			wantSave := cfg
			wantSave.Client.NICName, wantSave.Client.NICAddr = "USB LAN", "169.254.20.1"
			if len(requests) != 1 || !requests[0].Equal(wantSave) {
				t.Fatalf("adapter choice did not submit a save: %+v", requests)
			}

			state.Error = "Settings could not be saved."
			v.SetState(state)
			v.Complete(ActionSave)
			tt.Frame()
			if v.submitted != nil || !v.saved.Equal(cfg) || !v.draft.Equal(wantDraft) || v.dirty() != editPreference {
				t.Fatal("failed save did not restore the saved adapter while preserving other edits")
			}
			if v.canConnect() != !editPreference || !tt.HasText("Ethernet · 169.254.10.1") {
				t.Fatal("failed save did not restore the adapter selection and expected connect availability")
			}

			click(t, tt, "Adapter selection")
			click(t, tt, "USB LAN · 169.254.20.1")
			if len(requests) != 2 || !requests[1].Equal(wantSave) {
				t.Fatalf("choosing the same adapter did not retry the save: %+v", requests)
			}
		})
	}
}

func TestClientAdapterSelectionRequiresSaveAction(t *testing.T) {
	v := New(settings.Default(), Actions{})
	v.SetState(State{NICs: []carside.NIC{{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")}}})
	tt := ui.NewTester(v.Render, clientWidth, clientHeight)
	click(t, tt, "Adapter selection")
	if _, found := tt.Find("Ethernet · 169.254.10.1"); found {
		t.Fatal("adapter dropdown opened without a Save action")
	}
}

func TestOperatorInvitationAndLockedSettings(t *testing.T) {
	issued, starts, stops := 0, 0, 0
	var copied []string
	v := New(operatorConfig(), Actions{
		IssueInvite: func() { issued++ }, Copy: func(s string) { copied = append(copied, s) },
		Listen: func() { starts++ }, Stop: func() { stops++ }, Save: func(settings.Config) {},
	})
	tt := ui.NewTester(v.Render, 1080, 1500)
	click(t, tt, "Copy invitation")
	click(t, tt, "Generate invitation")
	click(t, tt, "Generate invitation")
	if issued != 1 || len(copied) != 0 {
		t.Fatal("invitation controls admitted a duplicate or implicit copy")
	}
	// Deliberately not a valid credential: never capture a real invitation in tests.
	code := "renet1:" + strings.Repeat("A", 1400)
	s := State{Invitation: code}
	v.SetState(s)
	tt.Frame()
	if len(copied) != 0 {
		t.Fatal("an incoming invitation implicitly copied to the clipboard")
	}
	click(t, tt, "Copy invitation")
	if len(copied) != 1 || copied[0] != code {
		t.Fatal("explicit copy did not preserve the entire invitation")
	}
	input(t, tt, "Generated invitation", "cannot edit")
	if v.state.Invitation != code {
		t.Fatal("generated invitation was editable")
	}
	click(t, tt, "Start operator")
	s.Running, s.Runtime.Phase = true, app.PhaseListening
	v.SetState(s)
	tt.Frame()
	openSettings(t, tt)
	input(t, tt, "Public endpoint", "cannot.example:47100")
	click(t, tt, "Exclusive diagnostic tool access")
	if _, ok := tt.Find("Workspace role"); ok {
		t.Fatal("operator settings exposed a role selector")
	}
	openSession(t, tt)
	click(t, tt, "Generate invitation")
	if starts != 1 || v.dirty() || v.draft.Role != settings.RoleOperator || issued != 2 {
		t.Fatal("running operator configuration changed or invite was unavailable while listening")
	}
	click(t, tt, "Stop runtime")
	click(t, tt, "Generate invitation")
	if stops != 1 || issued != 2 {
		t.Fatal("stopping admitted another invitation request")
	}
}

func TestInvalidFieldsAndSemanticNoOp(t *testing.T) {
	saves, starts, invites := 0, 0, 0
	v := New(operatorConfig(), Actions{Save: func(settings.Config) { saves++ }, Listen: func() { starts++ }, IssueInvite: func() { invites++ }})
	tt := ui.NewTester(v.Render, 1080, 1300)
	openSettings(t, tt)
	input(t, tt, "Diagnostic TCP", "06801")
	click(t, tt, "Save settings")
	if saves != 0 || v.dirty() {
		t.Fatal("a semantically unchanged configuration requested a save")
	}
	for _, bad := range []string{"0", "65536", "not-a-port", "6811"} {
		input(t, tt, "Diagnostic TCP", bad)
		click(t, tt, "Save settings")
		openSession(t, tt)
		click(t, tt, "Start operator")
		click(t, tt, "Generate invitation")
		if saves != 0 || starts != 0 || invites != 0 || !v.dirty() {
			t.Fatalf("invalid port %q admitted an action", bad)
		}
		openSettings(t, tt)
	}
	input(t, tt, "Diagnostic TCP", "6801")
	input(t, tt, "Public endpoint", "missing-port")
	click(t, tt, "Save settings")
	if saves != 0 || !tt.HasText("Public endpoint must be a valid IP or hostname followed by a port.") {
		t.Fatal("invalid endpoint did not block save with validation feedback")
	}
	missing := operatorConfig()
	missing.Operator.Endpoint, missing.Operator.Bind = "", ""
	v = New(missing, Actions{Listen: func() { starts++ }, IssueInvite: func() { invites++ }})
	tt = ui.NewTester(v.Render, 1080, 1300)
	if !tt.HasText("Operator settings") {
		t.Fatal("unconfigured operator did not start on Settings")
	}
	openSession(t, tt)
	click(t, tt, "Start operator")
	click(t, tt, "Generate invitation")
	if starts != 0 || invites != 0 {
		t.Fatal("incomplete saved configuration admitted an action")
	}
}

func TestUnknownAndMeasuredIndicators(t *testing.T) {
	v := New(operatorConfig(), Actions{})
	tt := ui.NewTester(v.Render, 1080, 1800)
	for _, fabricated := range []string{"0.0 ms", "0 B/s", "0 B", "safe", "voltage"} {
		if tt.HasText(fabricated) {
			t.Fatalf("unknown state fabricated %q", fabricated)
		}
	}
	s := State{Running: true, Runtime: app.Snapshot{
		Phase: app.PhaseConnected, VIN: "WBA12345678901234", Gateway: netip.MustParseAddr("169.254.1.1"),
		Peer: identity.Fingerprint{2}, RTTValid: true,
		Path:              transport.PathInfo{Kind: transport.PathRelay, Via: "hub.example:47000", RemoteAddr: "127.0.0.1:47000"},
		Metrics:           observe.Snapshot{At: time.Unix(1, 0), Totals: observe.Bytes{In: 2048, Out: 4096}, Rate: observe.Rate{Window: time.Second, InBytesPerSecond: 1024, OutBytesPerSecond: 0}, ActiveConnections: 2},
		MetricsIncomplete: true, Diagnostic: netip.MustParseAddrPort("127.0.0.2:6801"),
		Exclusive: exclusive.Snapshot{Enabled: true, Owner: exclusive.Identity{PID: 42, Created: 1}, Active: 2},
	}, TestResult: linktest.Result{RequestedSamples: 10, Samples: 5, Duration: time.Second, TimedOut: true, RTTP50: time.Millisecond, RTTP95: 2 * time.Millisecond},
		Events: []string{"Runtime started.", "Peer connected."}}
	v.SetState(s)
	tt.Frame()
	for _, text := range []string{"WBA12345678901234", "169.254.1.1", "0.0 ms", "2.0 KiB", "4.0 KiB", "1.0 KiB/s", "0 B/s", "partial", "1.0 ms", "5 / 10", "127.0.0.2:6801", "PID 42"} {
		if !tt.HasText(text) {
			t.Errorf("missing observed value %q", text)
		}
	}
	click(t, tt, "Log")
	if !tt.HasText("Peer connected.") {
		t.Fatal("runtime event was not visible in Log")
	}
	openSession(t, tt)
	s.TestResult = linktest.Result{Interrupted: true, Duration: time.Second}
	s.Error = "Peer does not support connection tests."
	v.SetState(s)
	tt.Frame()
	if !tt.HasText("interrupted") || !tt.HasText(s.Error) {
		t.Fatal("interruption or unsupported error was hidden")
	}
}

func TestRuntimePhases(t *testing.T) {
	v := New(settings.Default(), Actions{})
	tt := ui.NewTester(v.Render, 1080, 1000)
	for phase, want := range map[app.Phase]string{
		app.PhaseStarting: "Starting", app.PhaseWaitingNIC: "Waiting for adapter",
		app.PhaseDiscovering: "Discovering gateway", app.PhaseWaitingVehicle: "Waiting for gateway",
		app.PhaseConnecting: "Connecting", app.PhaseListening: "Listening", app.PhaseConnected: "Connected",
		app.PhaseDisconnected: "Disconnected", app.PhaseBackoff: "Retry scheduled", app.PhaseStopped: "Stopped",
	} {
		v.SetState(State{Running: phase != app.PhaseStopped, Runtime: app.Snapshot{Phase: phase, NextRetry: time.Unix(100, 0)}})
		tt.Frame()
		if !tt.HasText(want) {
			t.Errorf("phase %s did not show %q", phase, want)
		}
	}
}

func TestAccessibleStatusAndError(t *testing.T) {
	v := New(settings.Default(), Actions{})
	tt := ui.NewTester(v.Render, 1080, 1200)
	tt.UseScreenReader()
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseWaitingNIC}, Error: "Selected adapter is unavailable."})
	tt.Frame()
	if !strings.Contains(strings.Join(tt.Alerts(), " "), "Selected adapter is unavailable.") {
		t.Fatal("runtime error was not exposed as an accessibility alert")
	}
	if !strings.Contains(strings.Join(tt.Announcements(), " "), "Waiting for adapter") {
		t.Fatal("runtime stage was not exposed as an accessibility status")
	}
}

func TestEmptyDetailsDoNotShowTooltips(t *testing.T) {
	v := New(settings.Default(), Actions{})
	tt := ui.NewTester(func(c *ui.Context) {
		c.SetTheme(Theme())
		ui.Column(c).Fill().Padding(40).Gap(32).Children(func() {
			v.value(c, "Gateway IPv4", "")
			v.value(c, "Source IPv4", "169.254.10.1")
		})
	}, 420, 240)
	for _, sample := range []struct {
		display string
		wantTip bool
	}{{"—", false}, {"169.254.10.1", true}} {
		bounds, ok := tt.Find(sample.display)
		if !ok {
			t.Fatalf("detail %q is missing", sample.display)
		}
		tt.Move(bounds.X+bounds.W-4, bounds.Y+bounds.H/2)
		tt.Advance(time.Second)
		if _, shown := tt.AccessByRole(ui.RoleTooltip, ""); shown != sample.wantTip {
			t.Errorf("detail %q showed tooltip = %t, want %t", sample.display, shown, sample.wantTip)
		}
	}
}

func TestClientPasteAtMinimumSize(t *testing.T) {
	for _, zoom := range []float32{1, 1.5} {
		t.Run(fmt.Sprint(zoom), func(t *testing.T) {
			connected := ""
			v := New(settings.Default(), Actions{Connect: func(code string) { connected = code }})
			tt := ui.NewTester(v.Render, clientMinWidth, clientMinHeight)
			tt.SetZoom(zoom)
			for _, label := range []string{"Adapter selection", "Refresh adapters", "Invite or address:port and code", "Connect client", "Stop runtime"} {
				r, ok := tt.Find(label)
				if !ok || r.X < 0 || r.Y < 0 || r.W <= 0 || r.H <= 0 || r.X+r.W > clientMinWidth || r.Y+r.H > clientMinHeight {
					t.Errorf("primary control %q is outside the minimum window: %+v", label, r)
				}
			}
			click(t, tt, "Invite or address:port and code")
			code := "test-only-" + strings.Repeat("A", 1800)
			tt.SetClipboard(code)
			tt.Command("paste")
			r, ok := tt.Find("Invite or address:port and code")
			if !ok || r.X < 0 || r.X+r.W > clientMinWidth {
				t.Fatalf("invitation input overflowed: %+v", r)
			}
			click(t, tt, "Connect client")
			if connected != code {
				t.Fatal("minimum-size client did not connect with the complete pasted code")
			}
		})
	}
}

func TestResponsiveScrollAndLongInvitation(t *testing.T) {
	for _, width := range []int{1080, 900} {
		for _, zoom := range []float32{1, 1.5} {
			t.Run(fmt.Sprintf("%d/%.1f", width, zoom), func(t *testing.T) {
				copied := ""
				v := New(operatorConfig(), Actions{Copy: func(s string) { copied = s }})
				v.SetState(State{Invitation: "renet1:" + strings.Repeat("A", 1800), Events: []string{"A recent event."}})
				tt := ui.NewTester(v.Render, width, 680)
				tt.SetZoom(zoom)
				tt.SetScale(2)
				for _, label := range []string{"Settings", "Log", "Quit Remote ENET"} {
					r, ok := tt.Find(label)
					if !ok || r.X < 0 || r.X+r.W > float32(width) || r.Y < 0 || r.Y+r.H > 680 {
						t.Fatalf("header %q out of bounds: %+v", label, r)
					}
				}
				scrollTo(t, tt, "Copy invitation", float32(width), 680)
				click(t, tt, "Copy invitation")
				if copied != v.state.Invitation {
					t.Fatal("long invitation could not be copied after scrolling")
				}
				scrollTo(t, tt, "Generated invitation", float32(width), 680)
				r, ok := tt.Find("Generated invitation")
				if !ok || r.W <= 0 || r.X < 0 || r.X+r.W > float32(width) {
					t.Fatalf("long invitation overflowed: %+v", r)
				}
				click(t, tt, "Log")
				if !tt.HasText("A recent event.") {
					t.Fatal("events inaccessible after scrolling")
				}
			})
		}
	}
}

func TestPeerIDDetailsAndCopy(t *testing.T) {
	peer := identity.Fingerprint{1, 2, 3, 4, 5, 6}
	for _, entry := range []struct {
		name, button, back string
		operator, saved    bool
	}{
		{name: "status card", button: "Peer details", back: "Back to session"},
		{name: "current client", button: "Current client details", back: "Back to session", operator: true},
		{name: "remembered peer", button: "Peer details", back: "Back to settings", saved: true},
	} {
		for _, language := range []string{"en", "zh-Hans", "zh-Hant"} {
			for _, zoom := range []float32{1, 1.5} {
				t.Run(fmt.Sprintf("%s/%s/%.1f", entry.name, language, zoom), func(t *testing.T) {
					cfg := settings.Default()
					if entry.operator {
						cfg = operatorConfig()
					}
					if entry.saved {
						cfg.Client.Peer = peer.String()
					}
					var copied string
					v := New(cfg, Actions{Copy: func(s string) { copied = s }})
					v.SetLanguage(language)
					if !entry.saved {
						v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseConnected, Peer: peer}})
					}
					width, height := 680, 560
					if entry.operator {
						width, height = 900, 620
					}
					tt := ui.NewTester(v.Render, width, height)
					tt.SetZoom(zoom)
					if entry.saved {
						click(t, tt, v.tr("Settings"))
					}
					want := peer.Short()
					if entry.operator {
						want = v.tr("Current client · %s", want)
					}
					if !tt.HasText(want) || tt.HasText(peer.String()) {
						t.Fatalf("expected %q and no full peer ID", want)
					}
					button := v.tr(entry.button)
					scrollTo(t, tt, button, float32(width), float32(height))
					click(t, tt, button)
					if !tt.HasText(peer.String()) || copied != "" {
						t.Fatal("details should reveal the full peer ID without copying it")
					}
					click(t, tt, v.tr("Copy full peer ID"))
					if copied != peer.String() {
						t.Fatalf("copied %q, want %q", copied, peer)
					}
					click(t, tt, v.tr(entry.back))
					if !tt.HasText(want) || tt.HasText(peer.String()) {
						t.Fatal("back did not return to the short ID")
					}
					if entry.operator {
						scrollTo(t, tt, button, float32(width), float32(height))
						click(t, tt, button)
						v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseListening}})
						tt.Frame()
						if v.page != sessionPage || !v.detailsPeer.IsZero() || tt.HasText(peer.Short()) || tt.HasText(peer.String()) {
							t.Fatal("disconnect retained the client details")
						}
					}
				})
			}
		}
	}
}

func TestCenterReceiptsPreserveFormAndDirtyState(t *testing.T) {
	cfg := settings.Default()
	v := New(cfg, Actions{})
	cfg.Center = settings.Center{ID: "garage", API: []string{"https://garage.invalid"}}
	v.SetConfig(cfg)
	if v.dirty() {
		t.Fatal("center change dirtied clean form")
	}
	v.draft.AllowLinkTest = !cfg.AllowLinkTest
	submitted := v.draft
	v.submitted = &submitted
	cfg.Center.ID = "next"
	v.SetConfig(cfg)
	if !v.dirty() || v.submitted == nil || v.submitted.Center.ID != "next" || v.draft.Center.ID != "next" {
		t.Fatal("center receipt lost draft or pending submission")
	}
	cfg.AllowLinkTest = v.draft.AllowLinkTest
	v.SetConfig(cfg)
	if v.dirty() || v.submitted != nil {
		t.Fatal("matching receipt did not acknowledge save")
	}
}

func TestOperatorHubSettingsAndPath(t *testing.T) {
	cfg := settings.Default()
	cfg.Role = settings.RoleOperator
	cfg.Operator.Bind = "127.0.0.1"
	var copied string
	v := New(cfg, Actions{Copy: func(text string) { copied = text }, Listen: func() {}, IssueInvite: func() {}})
	v.page = settingsPage
	tt := ui.NewTester(v.Render, 1080, 1000)
	click(t, tt, "Connection mode")
	click(t, tt, "Self-hosted hub")
	if v.draft.Operator.Mode != "hub" || !tt.HasText("Hub fingerprint") {
		t.Fatal("mode selection did not expose hub fields")
	}
	input(t, tt, "Hub address", "hub.example:47000")
	input(t, tt, "Hub fingerprint", identity.Fingerprint{7}.String())
	input(t, tt, "Share address", "public.example:443")
	hub := v.draft.Operator.Hub
	click(t, tt, "Connection mode")
	click(t, tt, "Direct")
	click(t, tt, "Connection mode")
	click(t, tt, "Self-hosted hub")
	if v.draft.Operator.Hub != hub || hub.Share != "public.example:443" {
		t.Fatal("hub fields lost across mode switch")
	}
	v.SetIssuerEntry("public entry")
	tt.Frame()
	click(t, tt, "Copy entry")
	if copied != "public entry" {
		t.Fatal("wrong issuer entry copied")
	}
	fp := identity.Fingerprint{7}
	cfg.Operator.Mode = "hub"
	cfg.Operator.Hub = settings.Hub{Addr: "hub.invalid:443", FP: fp.String()}
	v = New(cfg, Actions{Listen: func() {}, IssueInvite: func() {}})
	if !v.canListen() || !v.canInvite() {
		t.Fatal("share address fallback unavailable")
	}
	for _, tc := range []struct{ message, title string }{
		{"", "Connecting to hub"},
		{"Hub has not authorized this device. Check the entry and the system time.", "Hub has not authorized this device"},
	} {
		v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseListening, LastError: tc.message}})
		if title, _, _ := v.stage(); title != tc.title {
			t.Fatalf("stage = %q, want %q", title, tc.title)
		}
	}
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseListening, HubWaiting: true}})
	tt = ui.NewTester(v.Render, 1080, 1000)
	if !tt.HasText("Waiting on hub") || !tt.HasText("Not configured") {
		t.Fatal("missing hub wait or center status")
	}
	const address = "workshop-operator-01.long-subdomain.dynamic-dns.example:47003"
	v.actions.Copy = func(text string) { copied = text }
	v.SetState(State{Running: true, Runtime: app.Snapshot{Phase: app.PhaseConnected, Path: transport.PathInfo{Kind: transport.PathRelay, Via: address, Hub: fp}}})
	for _, lang := range []string{"en", "zh-Hans", "zh-Hant"} {
		v.SetLanguage(lang)
		for _, width := range []int{900, 1080} {
			tt = ui.NewTester(v.Render, width, 760)
			if !tt.HasText(v.tr("Via hub %s", fp.Short())) || !tt.HasText(address) {
				t.Fatal("missing pinned hub path", lang, width)
			}
			copied = ""
			click(t, tt, v.tr("Copy hub address"))
			if copied != address {
				t.Fatal("incomplete hub address", lang, width)
			}
		}
	}
}
