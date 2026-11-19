package ui

import (
	"image/png"
	"net/netip"
	"os"
	"path/filepath"
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

// Match the desktop host's Client sizes in internal/desktopui/product.go.
const (
	clientWidth     = 440
	clientHeight    = 700
	clientMinWidth  = 420
	clientMinHeight = 480
)

// Preview fixtures contain no real invitations, device identities or vehicle data.
func previewState() State {
	return State{
		Running:    true,
		Invitation: "renet1:" + strings.Repeat("A", 600),
		NICs:       []carside.NIC{{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")}},
		Runtime: app.Snapshot{
			Phase: app.PhaseConnected, Generation: 1,
			NIC: carside.NIC{Name: "Ethernet", Address: netip.MustParseAddr("169.254.10.1")},
			VIN: "DEMO12345678901234", Gateway: netip.MustParseAddr("169.254.10.2"),
			Peer: identity.Fingerprint{1}, RTT: 18 * time.Millisecond, RTTValid: true,
			Path:       transport.PathInfo{Kind: transport.PathDirect, LocalAddr: "192.0.2.10:47000", RemoteAddr: "192.0.2.20:51000"},
			Metrics:    observe.Snapshot{At: time.Unix(100, 0), Totals: observe.Bytes{In: 530000, Out: 270000}, Rate: observe.Rate{Window: time.Second, InBytesPerSecond: 18432, OutBytesPerSecond: 4608}, ActiveConnections: 2},
			Diagnostic: netip.MustParseAddrPort("127.0.0.2:6801"), Control: netip.MustParseAddrPort("127.0.0.2:6811"), Discovery: netip.MustParseAddrPort("127.0.0.2:6811"),
			Exclusive: exclusive.Snapshot{Enabled: true, Owner: exclusive.Identity{PID: 4200, Created: 1}, Active: 2},
		},
		TestResult: linktest.Result{RequestedSamples: 10, Samples: 10, Duration: time.Second, RTTP50: 18 * time.Millisecond, RTTP95: 21 * time.Millisecond, PayloadDuration: time.Second, EchoedBytes: 1 << 20, EchoBytesPerSecond: 1 << 20},
		Events:     []string{"Runtime started.", "Peer connected.", "Connection test completed."},
	}
}

func clientPreviewState() State {
	state := previewState()
	state.Runtime.RTT = 999900 * time.Microsecond
	state.Runtime.Metrics.Totals = observe.Bytes{In: 10239 * (1 << 20) / 10, Out: 10238 * (1 << 20) / 10}
	state.Runtime.Metrics.Rate = observe.Rate{Window: time.Second, InBytesPerSecond: 1023.9 * (1 << 20), OutBytesPerSecond: 1023.8 * (1 << 20)}
	return state
}

func TestWorkspacePreview(t *testing.T) {
	dir := os.Getenv("RENET_UI_PREVIEW_DIR")
	if dir == "" {
		t.Skip("set RENET_UI_PREVIEW_DIR to export temporary review images")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	actions := Actions{Save: func(settings.Config) {}, Connect: func(string) {}, Listen: func() {}, Stop: func() {}, IssueInvite: func() {}, TestLink: func() {}, Copy: func(string) {}, Refresh: func() {}, Quit: func() {}}
	clientState := clientPreviewState()
	clientState.Notice = "Interfaces refreshed."
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		cfg := operatorConfig()
		cfg.Role = role
		cfg.Operator.Exclusive = role == settings.RoleOperator
		v := New(cfg, actions)
		state := previewState()
		width, height := clientWidth, clientHeight
		if role == settings.RoleOperator {
			width, height = 1080, 760
		} else {
			state = clientState
		}
		v.SetState(state)
		tt := ui.NewTester(v.Render, width, height)
		writePreview(t, tt, filepath.Join(dir, role+"-100.png"))
		tt.SetZoom(1.5)
		writePreview(t, tt, filepath.Join(dir, role+"-150.png"))
		tt.SetZoom(1)
		click(t, tt, "Settings")
		writePreview(t, tt, filepath.Join(dir, role+"-settings.png"))
		click(t, tt, "Log")
		writePreview(t, tt, filepath.Join(dir, role+"-log.png"))
	}

	cfg := settings.Default()
	cfg.Client.NICName, cfg.Client.NICAddr = "Ethernet", "169.254.10.1"
	client := New(cfg, actions)
	client.SetState(clientState)
	for _, language := range []string{"zh-Hans", "zh-Hant"} {
		client.SetLanguage(language)
		tt := ui.NewTester(client.Render, clientWidth, clientHeight)
		writePreview(t, tt, filepath.Join(dir, "client-"+language+"-100.png"))
	}
	client.SetLanguage("en")
	client.SetState(State{NICs: previewState().NICs})
	tt := ui.NewTester(client.Render, clientMinWidth, clientMinHeight)
	input(t, tt, "Invite or address:port and code", "renet1:DEMO")
	writePreview(t, tt, filepath.Join(dir, "client-min-100.png"))
	tt.SetZoom(1.5)
	writePreview(t, tt, filepath.Join(dir, "client-min-150.png"))
}

func writePreview(t *testing.T, tt *ui.Tester, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = png.Encode(f, tt.Image())
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
	t.Log(path)
}
