package desktop

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/app"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func isolatedConfigDirectory(t *testing.T) {
	t.Helper()
	base := t.TempDir()
	switch runtime.GOOS {
	case "windows":
		t.Setenv("AppData", base)
	case "darwin", "ios":
		t.Setenv("HOME", base)
	default:
		t.Setenv("XDG_CONFIG_HOME", base)
	}
}

func fixedController(t *testing.T, opts Options, deps dependencies) *Controller {
	t.Helper()
	c, err := newController(t.Context(), opts, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := c.Close(ctx); err != nil {
			t.Errorf("close fixed controller: %v", err)
		}
	})
	return c
}

func TestFixedRoleMissingDefaultsDoNotMigrateSharedConfig(t *testing.T) {
	isolatedConfigDirectory(t)
	sharedPath, stateRoot, err := settings.DefaultPaths()
	if err != nil {
		t.Fatal(err)
	}
	legacy := operatorSettings()
	legacy.Client.NICName = "legacy-choice"
	if err := settings.Save(sharedPath, legacy); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(sharedPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		c := fixedController(t, Options{FixedRole: role}, production())
		path, root, err := settings.DefaultPathsForRole(role)
		if err != nil || c.opts.ConfigPath != path || c.opts.StateDir != root || root != stateRoot || c.roleDir(role) != filepath.Join(stateRoot, role) {
			t.Fatal("fixed controller did not select isolated config and shared state root")
		}
		want := settings.Default()
		want.Role = role
		if c.Snapshot().Config != want {
			t.Fatal("missing configuration did not use role defaults")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("New created a missing configuration")
		}
		if err := c.Save(t.Context(), want); err != nil {
			t.Fatal(err)
		}
		if got, err := settings.Load(path); err != nil || got != want {
			t.Fatal("fixed configuration failed v1 round trip")
		}
	}
	if raw, err := os.ReadFile(sharedPath); err != nil || !bytes.Equal(raw, original) {
		t.Fatal("role defaults migrated or changed shared configuration")
	}
	generic := fixedController(t, Options{}, production())
	if generic.Snapshot().Config != legacy || generic.opts.ConfigPath != sharedPath {
		t.Fatal("generic library behavior changed")
	}
}

func TestFixedRoleExplicitPathsAndExistingConfigurationFailClosed(t *testing.T) {
	isolatedConfigDirectory(t)
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			for _, explicit := range []string{"config", "state", "both"} {
				opts := Options{FixedRole: role}
				defaultPath, defaultState, err := settings.DefaultPathsForRole(role)
				if err != nil {
					t.Fatal(err)
				}
				wantPath, wantState := defaultPath, defaultState
				if explicit != "state" {
					opts.ConfigPath = filepath.Join(t.TempDir(), "legacy-settings.json")
					wantPath = opts.ConfigPath
				}
				if explicit != "config" {
					opts.StateDir = filepath.Join(t.TempDir(), "existing-state")
					wantState = opts.StateDir
				}
				c := fixedController(t, opts, production())
				if c.opts.ConfigPath != wantPath || c.opts.StateDir != wantState || c.Snapshot().Config.Role != role {
					t.Fatal("explicit overrides did not preserve independent path defaults")
				}
			}
			path := filepath.Join(t.TempDir(), "existing.json")
			cfg := settings.Default()
			cfg.Role = role
			if err := settings.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			stateDir := filepath.Join(t.TempDir(), "state")
			opts := Options{ConfigPath: path, StateDir: stateDir, FixedRole: role}
			matching := fixedController(t, opts, production())
			if matching.Snapshot().Config != cfg {
				t.Fatal("matching explicit legacy configuration was not reused")
			}
			other := settings.RoleClient
			if role == other {
				other = settings.RoleOperator
			}
			cfg.Role = other
			if err := settings.Save(path, cfg); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if c, err := New(t.Context(), opts); !errors.Is(err, ErrRole) || c != nil {
				t.Fatalf("existing wrong role accepted: %v", err)
			}
			if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, original) {
				t.Fatal("wrong role configuration was rewritten")
			}
			if err := os.WriteFile(path, []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
			if c, err := New(t.Context(), opts); err == nil || c != nil {
				t.Fatal("corrupt configuration fell back to role defaults")
			}
			if raw, err := os.ReadFile(path); err != nil || string(raw) != "{" {
				t.Fatal("corrupt configuration was rewritten")
			}
			if _, err := os.Stat(stateDir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("rejected configuration created identity directories")
			}
		})
	}
	for _, role := range []string{"Client", "../operator", "unknown"} {
		opts := Options{FixedRole: role, ConfigPath: filepath.Join(t.TempDir(), "missing.json"), StateDir: t.TempDir()}
		if c, err := New(t.Context(), opts); !errors.Is(err, ErrRole) || c != nil {
			t.Fatalf("invalid fixed role was accepted: %v", err)
		}
	}
}

func TestFixedRoleRejectsRoleChangesAndForeignActions(t *testing.T) {
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		t.Run(role, func(t *testing.T) {
			cfg := clientSettings()
			if role == settings.RoleOperator {
				cfg = operatorSettings()
			}
			dir := t.TempDir()
			opts := Options{FixedRole: role, ConfigPath: filepath.Join(dir, "settings.json"), StateDir: filepath.Join(dir, "state")}
			if err := settings.Save(opts.ConfigPath, cfg); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(opts.ConfigPath)
			if err != nil {
				t.Fatal(err)
			}
			var factories, invites atomic.Int32
			f := newRuntime()
			deps := production()
			deps.newRuntime = func() appRuntime { factories.Add(1); return f }
			deps.issueInvite = func(context.Context, app.InviteConfig) (string, error) { invites.Add(1); return "code", nil }
			c := fixedController(t, opts, deps)
			wrong := cfg
			wrong.Role = settings.RoleClient
			if role == settings.RoleClient {
				wrong.Role = settings.RoleOperator
			}
			if err := c.Save(t.Context(), wrong); !errors.Is(err, ErrRole) {
				t.Fatalf("save changed fixed role: %v", err)
			}
			if s := c.Snapshot(); s.Config != cfg || s.Error == "" || s.Notice != "" {
				t.Fatal("rejected save changed baseline or failed to publish error")
			}
			if raw, err := os.ReadFile(opts.ConfigPath); err != nil || !bytes.Equal(raw, original) {
				t.Fatal("rejected role change wrote to disk")
			}
			if role == settings.RoleClient {
				if err := c.StartOperator(t.Context()); !errors.Is(err, ErrRole) {
					t.Fatal(err)
				}
				if code, err := c.IssueInvite(t.Context()); !errors.Is(err, ErrRole) || code != "" {
					t.Fatal("client controller issued an invitation")
				}
			} else if err := c.StartClient(t.Context(), ""); !errors.Is(err, ErrRole) {
				t.Fatal(err)
			}
			if factories.Load() != 0 || invites.Load() != 0 {
				t.Fatal("wrong-role action reached app")
			}
			if role == settings.RoleClient {
				err = c.StartClient(t.Context(), "")
			} else {
				if _, err := c.IssueInvite(t.Context()); err != nil {
					t.Fatal(err)
				}
				err = c.StartOperator(t.Context())
			}
			if err != nil {
				t.Fatal(err)
			}
			call := await(t, f.started)
			if role == settings.RoleClient && call.client.StateDir != filepath.Join(opts.StateDir, role) || role == settings.RoleOperator && call.operator.StateDir != filepath.Join(opts.StateDir, role) {
				t.Fatal("fixed runtime used the wrong identity directory")
			}
			if err := c.Stop(t.Context()); err != nil {
				t.Fatal(err)
			}
			cfg.AllowLinkTest = !cfg.AllowLinkTest
			if err := c.Save(t.Context(), cfg); err != nil || c.Snapshot().Config != cfg {
				t.Fatal("fixed role rejected its own configuration")
			}
		})
	}
}

func TestFixedControllersRunInParallelWithoutSharingPreferencesOrIdentities(t *testing.T) {
	isolatedConfigDirectory(t)
	clientPath, stateRoot, err := settings.DefaultPathsForRole(settings.RoleClient)
	if err != nil {
		t.Fatal(err)
	}
	operatorPath, operatorRoot, err := settings.DefaultPathsForRole(settings.RoleOperator)
	if err != nil || operatorRoot != stateRoot {
		t.Fatal("fixed roles do not share the same state root")
	}
	clientCfg, operatorCfg := settings.Default(), operatorSettings()
	if err := settings.Save(clientPath, clientCfg); err != nil {
		t.Fatal(err)
	}
	if err := settings.Save(operatorPath, operatorCfg); err != nil {
		t.Fatal(err)
	}
	clientStore, err := trust.Open(t.Context(), filepath.Join(stateRoot, settings.RoleClient))
	if err != nil {
		t.Fatal(err)
	}
	clientIdentity, err := clientStore.LoadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	operatorStore, err := trust.Open(t.Context(), filepath.Join(stateRoot, settings.RoleOperator))
	if err != nil {
		t.Fatal(err)
	}
	operatorIdentity, err := operatorStore.LoadIdentity(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if clientIdentity.Fingerprint() == operatorIdentity.Fingerprint() {
		t.Fatal("roles share one identity")
	}
	if err := clientStore.SavePeer(t.Context(), transport.PeerInfo{ID: operatorIdentity.Fingerprint(), Endpoints: []transport.Endpoint{{Kind: "hub", Addr: operatorCfg.Operator.Endpoint, Hub: operatorIdentity.Fingerprint()}}}); err != nil {
		t.Fatal(err)
	}
	clientRuntime, operatorRuntime := newRuntime(), newRuntime()
	clientDeps, operatorDeps := production(), production()
	clientDeps.newRuntime = func() appRuntime { return clientRuntime }
	operatorDeps.newRuntime = func() appRuntime { return operatorRuntime }
	client := fixedController(t, Options{FixedRole: settings.RoleClient}, clientDeps)
	operator := fixedController(t, Options{FixedRole: settings.RoleOperator}, operatorDeps)
	clientCfg.Client.NICName = "client-choice"
	operatorCfg.Operator.Bind = "127.0.0.2"
	start, results := make(chan struct{}), make(chan error, 2)
	go func() { <-start; results <- client.Save(t.Context(), clientCfg) }()
	go func() { <-start; results <- operator.Save(t.Context(), operatorCfg) }()
	close(start)
	for range cap(results) {
		if err := await(t, results); err != nil {
			t.Fatal(err)
		}
	}
	code, err := operator.IssueInvite(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	trustBefore := make(map[string][]byte)
	for _, role := range []string{settings.RoleClient, settings.RoleOperator} {
		path := filepath.Join(stateRoot, role, "trust.json")
		trustBefore[path], err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	start = make(chan struct{})
	go func() { <-start; results <- client.StartClient(t.Context(), code) }()
	go func() { <-start; results <- operator.StartOperator(t.Context()) }()
	close(start)
	for range cap(results) {
		if err := await(t, results); err != nil {
			t.Fatal(err)
		}
	}
	clientCall, operatorCall := await(t, clientRuntime.started), await(t, operatorRuntime.started)
	if clientCall.client.StateDir != filepath.Join(stateRoot, settings.RoleClient) || operatorCall.operator.StateDir != filepath.Join(stateRoot, settings.RoleOperator) {
		t.Fatal("runtime paths added or removed a role directory")
	}
	clientState, operatorState := connected(1), connected(1)
	clientState.Peer, clientState.Device = operatorIdentity.Fingerprint(), clientIdentity.Fingerprint()
	operatorState.Role, operatorState.Peer = session.RoleOperator, clientIdentity.Fingerprint()
	clientRuntime.emit(clientState)
	operatorRuntime.emit(operatorState)
	stateWhere(t, client, func(s State) bool { return s.Config.Client.Peer == operatorIdentity.Fingerprint().String() })
	stateWhere(t, operator, func(s State) bool { return s.Runtime.Phase == app.PhaseConnected })
	if got, err := settings.Load(operatorPath); err != nil || got != operatorCfg || operator.Snapshot().Config != operatorCfg {
		t.Fatal("client pairing or save changed operator preferences")
	}
	clientCfg.Client.Peer = operatorIdentity.Fingerprint().String()
	if got, err := settings.Load(clientPath); err != nil || got != clientCfg {
		t.Fatal("parallel saves or pairing lost client preferences")
	}
	if err := client.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !operator.Snapshot().Running || operatorCall.ctx.Err() != nil {
		t.Fatal("stopping client canceled the parallel operator")
	}
	if err := operator.Stop(t.Context()); err != nil {
		t.Fatal(err)
	}
	for path, before := range trustBefore {
		if after, err := os.ReadFile(path); err != nil || !bytes.Equal(after, before) {
			t.Fatal("configuration operations changed trust records")
		}
	}
	retainedClient, err := clientStore.LoadIdentity(t.Context())
	if err != nil || retainedClient.Fingerprint() != clientIdentity.Fingerprint() {
		t.Fatal("client identity was moved or regenerated")
	}
	retainedOperator, err := operatorStore.LoadIdentity(t.Context())
	if err != nil || retainedOperator.Fingerprint() != operatorIdentity.Fingerprint() {
		t.Fatal("operator identity was moved or regenerated")
	}
}
