package desktop

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/diagnostics"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

func TestCenterSurvivesStaleForm(t *testing.T) {
	old := clientSettings()
	var persisted settings.Config
	c := testController(t, old, func(d *dependencies) {
		d.save = func(_ string, cfg settings.Config) error { persisted = cfg; return nil }
	})
	center := settings.Center{ID: "garage", API: []string{"https://garage.invalid"}}
	if err := c.SetCenter(t.Context(), center); err != nil {
		t.Fatal(err)
	}
	old.AllowLinkTest = !old.AllowLinkTest
	if err := c.Save(t.Context(), old); err != nil {
		t.Fatal(err)
	}
	old.Center = center
	if !persisted.Equal(old) || !c.Snapshot().Config.Equal(old) {
		t.Fatal("stale form replaced center or lost preferences")
	}
	if err := c.StartClient(t.Context(), "12345678"); !errors.Is(err, errCenterCode) {
		t.Fatal("configured center allowed unqualified numeric input")
	}
	snapshot := c.Snapshot()
	snapshot.Config.Center.API[0] = "mutated"
	if !c.Snapshot().Config.Equal(old) {
		t.Fatal("shared center API list")
	}
}

func TestHubInvitationAndSubscription(t *testing.T) {
	cfg := operatorSettings()
	cfg.Operator.Mode = "hub"
	cfg.Operator.Hub = settings.Hub{Addr: "hub.invalid:443", FP: identity.Fingerprint{7}.String()}
	cfg.Operator.Endpoint = ""
	c := testController(t, cfg, func(*dependencies) {})
	states := c.Subscribe(t.Context())
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)
	code, err := c.IssueInvite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := invite.ParseInput(code)
	if err != nil {
		t.Fatal(err)
	}
	i := parsed.Invite
	ep := i.Peer.Endpoints[0]
	if i.Version != 2 || ep.Addr != cfg.Operator.Hub.Addr || !pairedEndpoint(ep, i.Peer.ID, ep.Addr) || time.Until(i.ExpiresAt) > 24*time.Hour {
		t.Fatal("invalid standalone hub invitation")
	}
	state := await(t, states)
	report, err := diagnostics.Build(diagnostics.Input{Version: "1.0.0", Role: cfg.Role, Runtime: state.Runtime, Permission: diagnostics.Permission{Platform: runtime.GOOS}})
	if err != nil {
		t.Fatal(err)
	}
	output := fmt.Sprint(state, c.Snapshot()) + string(report) + logs.String()
	if strings.Contains(output, code) || strings.Contains(output, ep.Ticket) {
		t.Fatal("credential in subscription or diagnostics")
	}
	entry, err := c.IssuerEntry(t.Context())
	if err != nil || !strings.Contains(entry, i.Peer.ID.String()) {
		t.Fatal("issuer entry does not identify operator")
	}
	ep.Ticket = ""
	if pairedEndpoint(ep, i.Peer.ID, ep.Addr) {
		t.Fatal("accepted standalone hub without a ticket")
	}
	if pairedEndpoint(transport.Endpoint{Kind: "hub", Addr: ep.Addr, Hub: i.Peer.ID, Ticket: "unexpected"}, i.Peer.ID, ep.Addr) {
		t.Fatal("accepted ticket for embedded hub")
	}
}
