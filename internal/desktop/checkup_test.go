package desktop

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/checkup"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
)

func checkedReport() checkup.Report {
	now := time.Now()
	return checkup.Report{StartedAt: now, FinishedAt: now, Complete: true,
		Findings: []checkup.Finding{{ID: "settings", Status: checkup.Pass, Code: "settings_valid"}}}
}

func TestCheckEndpointsDoesNotPairOrPersist(t *testing.T) {
	peer := transport.PeerInfo{ID: identity.Fingerprint{7}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "fixture.invalid:47000", Hub: identity.Fingerprint{8}}}}
	var inputs []checkup.Input
	reads := 0
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.readPeer = func(context.Context, string, identity.Fingerprint) (transport.PeerInfo, error) {
			reads++
			return peer, nil
		}
		d.checkup = func(_ context.Context, in checkup.Input) checkup.Report {
			inputs = append(inputs, in)
			return checkedReport()
		}
		d.save = func(string, settings.Config) error { t.Fatal("self-check persisted settings"); return nil }
	})
	if err := c.CheckConnection(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || !reflect.DeepEqual(inputs[0].Endpoints, peer.Endpoints) {
		t.Fatal("saved public endpoints not used")
	}
	code, err := (invite.Invite{Version: 1, Peer: peer, Token: [16]byte{9}, ExpiresAt: time.Now().Add(time.Minute)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.CheckConnection(t.Context(), "\uFEFF "+code+"\n"); err != nil {
		t.Fatal(err)
	}
	if reads != 1 || !reflect.DeepEqual(inputs[1].Endpoints, peer.Endpoints) {
		t.Fatal("invitation was not decoded independently")
	}
	if err := c.CheckConnection(t.Context(), "renet1:SECRET"); err != nil {
		t.Fatal(err)
	}
	if inputs[2].PeerCode != "invite_invalid" || len(inputs[2].Endpoints) != 0 {
		t.Fatal("invalid invitation fell back to another peer")
	}
	if strings.Contains(strings.Join(c.Snapshot().Events, "\n"), "renet1:") || c.Snapshot().Invitation != "" {
		t.Fatal("invitation leaked into state")
	}
	if _, err := os.Stat(c.opts.StateDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("self-check created state: %v", err)
	}
	s := c.Snapshot()
	s.Checkup.Findings[0].Code = "mutated"
	if c.Snapshot().Checkup.Findings[0].Code != "settings_valid" {
		t.Fatal("snapshot retained a borrowed report slice")
	}
}

func TestCheckCancellationJoinsAndStartWaitsForIdleSockets(t *testing.T) {
	for _, action := range []string{"cancel", "stop", "close", "save", "start"} {
		t.Run(action, func(t *testing.T) {
			entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			f := newRuntime()
			c := testController(t, operatorSettings(), func(d *dependencies) {
				d.newRuntime = func() appRuntime { return f }
				d.checkup = func(ctx context.Context, in checkup.Input) checkup.Report {
					if in.Running {
						t.Error("idle check became an active check")
					}
					close(entered)
					<-ctx.Done()
					close(canceled)
					<-release
					return checkedReport()
				}
			})
			t.Cleanup(unblock)
			done := make(chan error, 1)
			go func() { done <- c.CheckConnection(t.Context(), "") }()
			await(t, entered)
			if err := c.CheckConnection(t.Context(), ""); !errors.Is(err, ErrBusy) {
				t.Fatalf("parallel check: %v", err)
			}
			operation := make(chan error, 1)
			switch action {
			case "cancel":
				operation <- c.CancelChecks(canceledContext())
			case "stop":
				operation <- c.Stop(canceledContext())
			case "close":
				operation <- c.Close(canceledContext())
			case "save":
				cfg := c.Snapshot().Config
				cfg.AllowLinkTest = !cfg.AllowLinkTest
				go func() { operation <- c.Save(t.Context(), cfg) }()
			case "start":
				go func() { operation <- c.StartOperator(t.Context()) }()
			}
			await(t, canceled)
			if !c.Snapshot().Checking || c.Snapshot().Running {
				t.Fatal("check released before cleanup joined")
			}
			select {
			case <-f.started:
				t.Fatal("runtime opened listeners before idle probes joined")
			default:
			}
			unblock()
			if err := await(t, done); !errors.Is(err, context.Canceled) {
				t.Fatalf("check result: %v", err)
			}
			result := await(t, operation)
			if action == "save" || action == "start" {
				if result != nil {
					t.Fatal(result)
				}
			} else if !errors.Is(result, context.Canceled) {
				t.Fatalf("abandoned wait: %v", result)
			}
			if action == "start" {
				await(t, f.started)
			}
			if action == "close" {
				if err := c.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			state := c.Snapshot()
			if state.Checking {
				t.Fatal("checking latch remained set")
			}
			if action == "cancel" {
				if !state.Checkup.Canceled || state.Checkup.Complete {
					t.Fatal("missing partial cancellation report")
				}
			} else if !state.Checkup.StartedAt.IsZero() {
				t.Fatalf("stale report survived %s", action)
			}
		})
	}
}

func TestPendingCheckWaitingForSettingsGateCanBeStopped(t *testing.T) {
	c := testController(t, operatorSettings(), func(d *dependencies) {
		d.checkup = func(context.Context, checkup.Input) checkup.Report {
			t.Fatal("canceled pending check ran")
			return checkup.Report{}
		}
	})
	if err := c.acquire(t.Context()); err != nil {
		t.Fatal(err)
	}
	defer c.release()
	done := make(chan error, 1)
	go func() { done <- c.CheckConnection(t.Context(), "") }()
	stateWhere(t, c, func(s State) bool { return s.Checking })
	if err := c.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestRunningChecksRejectNewGenerationWithoutStoppingSession(t *testing.T) {
	f := newRuntime()
	entered := make(chan checkup.Input, 1)
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { return f }
		d.readPeer = func(context.Context, string, identity.Fingerprint) (transport.PeerInfo, error) {
			return transport.PeerInfo{}, os.ErrNotExist
		}
		d.checkup = func(ctx context.Context, in checkup.Input) checkup.Report {
			entered <- in
			<-ctx.Done()
			return checkedReport()
		}
	})
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	f.emit(connected(1))
	stateWhere(t, c, func(s State) bool { return s.Runtime.Generation == 1 })
	done := make(chan error, 1)
	go func() { done <- c.CheckConnection(t.Context(), "") }()
	in := await(t, entered)
	if !in.Running || in.Runtime.Generation != 1 {
		t.Fatal("check borrowed the wrong runtime")
	}
	f.emit(connected(2))
	if err := await(t, done); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	s := stateWhere(t, c, func(s State) bool { return !s.Checking && s.Runtime.Generation == 2 })
	if !s.Checkup.StartedAt.IsZero() || !s.Running {
		t.Fatal("old report was published")
	}
	select {
	case <-f.canceled:
		t.Fatal("checks ended the healthy peer session")
	default:
	}
}

func TestRememberedPairingInvalidatesCompletedChecks(t *testing.T) {
	f := newRuntime()
	saving, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	c := testController(t, clientSettings(), func(d *dependencies) {
		d.newRuntime = func() appRuntime { return f }
		d.readPeer = func(context.Context, string, identity.Fingerprint) (transport.PeerInfo, error) {
			return transport.PeerInfo{}, os.ErrNotExist
		}
		d.checkup = func(context.Context, checkup.Input) checkup.Report { return checkedReport() }
		d.save = func(string, settings.Config) error { close(saving); <-release; return nil }
	})
	t.Cleanup(unblock)
	if err := c.StartClient(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	await(t, f.started)
	s := connected(1)
	s.Peer = identity.Fingerprint{9}
	f.emit(s)
	await(t, saving)
	// A running check does not block behind unrelated pairing persistence.
	if err := c.CheckConnection(t.Context(), ""); err != nil {
		t.Fatal(err)
	}
	if c.Snapshot().Checkup.StartedAt.IsZero() {
		t.Fatal("checks could not complete while pairing was being saved")
	}
	unblock()
	latest := stateWhere(t, c, func(state State) bool { return state.Config.Client.Peer == s.Peer.String() })
	if !latest.Checkup.StartedAt.IsZero() {
		t.Fatal("report for the old saved peer survived persistence")
	}
}
