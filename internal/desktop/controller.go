// Package desktop owns GUI operations without depending on a window toolkit.
package desktop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/checkup"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/netpriv"
	"remote-enet/internal/pairing"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

// Admission errors are stable across runtime instances and can be matched
// without interpreting product-facing State.Error text.
var (
	ErrBusy   = errors.New("desktop: operation busy")
	ErrClosed = errors.New("desktop: controller closed")
	ErrRole   = errors.New("desktop: saved role does not match operation")
)

// Options uses the platform's configuration directory for omitted paths.
type Options struct {
	ConfigPath, StateDir string
	// FixedRole is client or operator; empty preserves generic behavior.
	FixedRole string
	// AppVersion is the executable's build version used for center requests.
	AppVersion string
	// DirectFactory confines platform authorization to an explicit Operator action.
	DirectFactory app.DirectFactory
	// NetworkStatus only inspects capabilities; it must never request authorization.
	NetworkStatus func() netpriv.Status
}

// State is an independent, in-memory view. Only Config is persisted;
// Invitation is an explicitly requested secret and must never be logged.
type State struct {
	Center                     CenterView
	Config                     settings.Config
	Runtime                    app.Snapshot
	Capture                    hsfzcapture.Snapshot
	Running, Stopping, Testing bool
	DirectWorking              bool
	Invitation, Notice, Error  string
	// HubInviteDigest lets the GUI discard a late issuance result after Stop or Save.
	HubInviteDigest [32]byte
	NumericCode     pairing.Status
	PairingHint     string
	NICs            []carside.NIC
	TestResult      linktest.Result
	Events          []string
	Checking        bool
	Checkup         checkup.Report
}

type appRuntime interface {
	AnswerSAS(context.Context, uint64, bool) error
	EndReception(context.Context) error
	RetryPairing(context.Context) error
	CenterDisplay() (string, centerflow.SASPrompt)
	CenterAccount(context.Context, string, app.CenterConfig) (*centerflow.Operator, error)
	RunClient(context.Context, app.ClientConfig) error
	RunOperator(context.Context, app.OperatorConfig) error
	Snapshot() app.Snapshot
	Subscribe(context.Context) <-chan app.Snapshot
	TestLinkForGeneration(context.Context, uint64, linktest.Options) (linktest.Result, error)
	IssueNumericCode(context.Context) (pairing.Display, error)
	NumericCodeStatus() pairing.Status
	NumericCodeDisplay(context.Context, uint64) (pairing.Display, error)
	RevokeNumericCode(context.Context, uint64) error
}

type dependencies struct {
	newRuntime  func() appRuntime
	load        func(string) (settings.Config, error)
	save        func(string, settings.Config) error
	interfaces  func(context.Context) ([]carside.NIC, error)
	issueInvite func(context.Context, app.InviteConfig) (string, error)
	pairedPeer  func(context.Context, string, transport.PeerInfo) error
	readPeer    func(context.Context, string, identity.Fingerprint) (transport.PeerInfo, error)
	checkup     func(context.Context, checkup.Input) checkup.Report
	checkCenter func(ctx context.Context, cfg settings.Center, role, appVersion string) []center.EntryResult
}

func production() dependencies {
	return dependencies{
		newRuntime: func() appRuntime { return app.New() },
		load:       settings.Load, save: settings.Save,
		interfaces: func(ctx context.Context) ([]carside.NIC, error) {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return carside.Interfaces()
		},
		issueInvite: app.IssueInvite,
		readPeer:    trust.ReadPeer,
		checkup:     checkup.Run,
		checkCenter: center.Check,
		pairedPeer: func(ctx context.Context, dir string, expected transport.PeerInfo) error {
			store, err := trust.Open(ctx, dir)
			if err != nil {
				return err
			}
			peer, err := store.Peer(ctx, expected.ID)
			if err != nil {
				return err
			}
			if peer.ID != expected.ID || !slices.Equal(peer.Endpoints, expected.Endpoints) {
				return trust.ErrNotFound
			}
			return nil
		},
	}
}

type subscription struct {
	ch   chan State
	stop func() bool
}

// Controller serializes configuration I/O separately from its state lock.
// A run remains reserved until its runtime, subscription and borrowed workers
// have all exited, including when a Stop caller abandons its wait.
type Controller struct {
	mu             sync.Mutex
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	closed         bool
	opts           Options
	deps           dependencies
	state          State
	run            *running
	invite         *issuingInvite
	check          *checkingConnection
	subs           map[*subscription]struct{}
	gate           chan struct{}
	ops            sync.WaitGroup
	stopEpoch      uint64
	codeGeneration uint64
}

// New's context owns all subsequent runtimes. Individual request contexts do
// not own a runtime after Start has accepted the request.
func New(ctx context.Context, opts Options) (*Controller, error) {
	return newController(ctx, opts, production())
}

func newController(ctx context.Context, opts Options, deps dependencies) (*Controller, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.FixedRole != "" && opts.FixedRole != settings.RoleClient && opts.FixedRole != settings.RoleOperator {
		return nil, fmt.Errorf("desktop: fixed role must be client or operator: %w", ErrRole)
	}
	if opts.ConfigPath == "" || opts.StateDir == "" {
		paths := settings.DefaultPaths
		if opts.FixedRole != "" {
			paths = func() (string, string, error) { return settings.DefaultPathsForRole(opts.FixedRole) }
		}
		configPath, stateDir, err := paths()
		if err != nil {
			return nil, fmt.Errorf("desktop: default paths: %w", err)
		}
		if opts.ConfigPath == "" {
			opts.ConfigPath = configPath
		}
		if opts.StateDir == "" {
			opts.StateDir = stateDir
		}
	}
	cfg, err := loadConfig(opts, deps.load)
	if err != nil {
		return nil, fmt.Errorf("desktop: load settings: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("desktop: validate settings: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &Controller{ctx: ctx, cancel: cancel, done: make(chan struct{}), opts: opts, deps: deps,
		state: State{Config: cfg, Runtime: app.Snapshot{Phase: app.PhaseIdle}},
		subs:  make(map[*subscription]struct{}), gate: make(chan struct{}, 1)}
	go c.shutdown()
	return c, nil
}

func loadConfig(opts Options, load func(string) (settings.Config, error)) (settings.Config, error) {
	cfg, err := load(opts.ConfigPath)
	if err != nil {
		return settings.Config{}, err
	}
	if opts.FixedRole != "" {
		// Load validates missing ancestors first: Windows can also report an
		// existing non-directory parent as os.ErrNotExist.
		_, err := os.Lstat(opts.ConfigPath)
		if errors.Is(err, os.ErrNotExist) {
			cfg := settings.Default()
			cfg.Role = opts.FixedRole
			return cfg, nil
		}
		if err != nil {
			return settings.Config{}, fmt.Errorf("inspect fixed-role configuration: %w", err)
		}
	}
	if opts.FixedRole != "" && cfg.Role != opts.FixedRole {
		return settings.Config{}, fmt.Errorf("fixed-role configuration mismatch: %w", ErrRole)
	}
	return cfg, nil
}

func (c *Controller) allowsRole(role string) bool {
	return c.opts.FixedRole == "" || c.opts.FixedRole == role
}

// Snapshot may be freely modified by the caller, including nested metrics.
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshCaptureLocked()
	c.refreshNumericCodeLocked()
	return cloneState(c.state)
}

// Subscribe immediately sends a snapshot; unread updates are replaced. Each
// subscriber owns its data. Cancellation and Close close the capacity-one channel.
func (c *Controller) Subscribe(ctx context.Context) <-chan State {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refreshCaptureLocked()
	c.refreshNumericCodeLocked()
	sub := &subscription{ch: make(chan State, 1)}
	if ctx.Err() != nil {
		close(sub.ch)
		return sub.ch
	}
	sub.ch <- cloneState(c.state)
	select {
	case <-c.done:
		close(sub.ch)
		return sub.ch
	default:
	}
	c.subs[sub] = struct{}{}
	sub.stop = context.AfterFunc(ctx, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if _, ok := c.subs[sub]; ok {
			delete(c.subs, sub)
			close(sub.ch)
		}
	})
	return sub.ch
}

// Save merges the persisted peer and center under the configuration gate.
func (c *Controller) Save(ctx context.Context, cfg settings.Config) error {
	return c.saveConfig(ctx, cfg, nil)
}

// SetCenter changes the persisted center independently of any pending form edits.
func (c *Controller) SetCenter(ctx context.Context, center settings.Center) error {
	center.API = slices.Clone(center.API)
	return c.saveConfig(ctx, settings.Config{}, &center)
}

// IssuerEntry returns the public authorization entry for this operator identity.
func (c *Controller) IssuerEntry(ctx context.Context) (string, error) {
	ctx, finish, _, err := c.operation(ctx, false)
	if err != nil {
		return "", err
	}
	defer finish()
	if err := c.acquire(ctx); err != nil {
		return "", err
	}
	defer c.release()
	if c.Snapshot().Config.Role != settings.RoleOperator {
		return "", ErrRole
	}
	return app.IssuerEntry(ctx, c.roleDir(settings.RoleOperator), "operator")
}

func (c *Controller) saveConfig(ctx context.Context, cfg settings.Config, center *settings.Center) error {
	ctx, finish, _, err := c.operation(ctx, true)
	if err != nil {
		c.failure("Settings could not be saved.", err)
		return err
	}
	defer finish()
	c.mu.Lock()
	if c.check != nil {
		c.check.cancel()
	}
	c.mu.Unlock()
	if err := c.acquire(ctx); err != nil {
		c.failure("Settings could not be saved.", err)
		return err
	}
	defer c.release()
	c.mu.Lock()
	err = c.availableLocked(ctx, true)
	if center != nil {
		cfg = c.state.Config
		cfg.Center = *center
	} else {
		cfg.Center = c.state.Config.Center
	}
	if err == nil && !c.allowsRole(cfg.Role) {
		err = ErrRole
	}
	cfg.Client.Peer = c.state.Config.Client.Peer
	c.mu.Unlock()
	if err == nil {
		err = cfg.Validate()
	}
	if err == nil {
		err = c.deps.save(c.opts.ConfigPath, cfg)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err != nil {
		c.failureLocked("Settings could not be saved.", err)
		return fmt.Errorf("desktop: save settings: %w", err)
	}
	// Save cannot interrupt an atomic rename. A successful write is reflected
	// even if cancellation raced with it; it must not report a fictitious rollback.
	if cfg.Center.ID != c.state.Config.Center.ID || !slices.Equal(cfg.Center.API, c.state.Config.Center.API) || cfg.Role != c.state.Config.Role || cfg.Operator.Mode != c.state.Config.Operator.Mode {
		c.state.Center = CenterView{}
		c.state.Runtime.Center = app.CenterSnapshot{}
	}
	c.state.Config = cfg
	if c.check != nil {
		c.check.cancel()
	}
	c.state.Checkup = checkup.Report{}
	c.state.Invitation = ""
	c.state.NumericCode = pairing.Status{}
	c.successLocked("Settings saved.")
	return nil
}

// RefreshNICs never substitutes a missing saved interface selection.
func (c *Controller) RefreshNICs(ctx context.Context) error {
	ctx, finish, _, err := c.operation(ctx, false)
	if err != nil {
		return err
	}
	defer finish()
	nics, err := c.deps.interfaces(ctx)
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrClosed
	}
	if err != nil {
		c.failureLocked("Interfaces could not be refreshed.", err)
		return fmt.Errorf("desktop: refresh interfaces: %w", err)
	}
	c.state.NICs = append([]carside.NIC(nil), nics...)
	c.successLocked("Interfaces refreshed.")
	return nil
}

func (c *Controller) availableLocked(ctx context.Context, idle bool) error {
	if c.closed || c.ctx.Err() != nil {
		return ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if idle && c.run != nil {
		return ErrBusy
	}
	return nil
}

// operation admits short I/O requests before shutdown starts waiting. The
// state lock prevents WaitGroup.Add racing with a zero-count shutdown Wait.
func (c *Controller) operation(ctx context.Context, idle bool) (context.Context, func(), uint64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.operationLocked(ctx, idle)
}

func (c *Controller) operationLocked(ctx context.Context, idle bool) (context.Context, func(), uint64, error) {
	if err := c.availableLocked(ctx, idle); err != nil {
		return nil, nil, 0, err
	}
	c.ops.Add(1)
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(c.ctx, cancel)
	return opCtx, func() { stop(); cancel(); c.ops.Done() }, c.stopEpoch, nil
}

func (c *Controller) acquire(ctx context.Context) error {
	select {
	case c.gate <- struct{}{}:
		if err := ctx.Err(); err != nil {
			c.release()
			return err
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *Controller) release() { <-c.gate }

func (c *Controller) roleDir(role string) string { return filepath.Join(c.opts.StateDir, role) }

// Close permanently rejects new operations immediately. A canceled wait does
// not abandon cleanup; later Close calls can join the same cleanup.
func (c *Controller) Close(ctx context.Context) error {
	c.mu.Lock()
	c.closeLocked()
	c.mu.Unlock()
	return wait(ctx, c.done)
}

func (c *Controller) closeLocked() {
	c.closed = true
	c.state.Invitation = ""
	c.stopLocked()
	c.cancel()
}

func (c *Controller) shutdown() {
	<-c.ctx.Done()
	c.mu.Lock()
	c.closeLocked()
	run := c.run
	c.mu.Unlock()
	c.ops.Wait()
	if run != nil {
		<-run.done
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.publishLocked()
	for sub := range c.subs {
		sub.stop()
		close(sub.ch)
		delete(c.subs, sub)
	}
	close(c.done)
}

func wait(ctx context.Context, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	default:
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
