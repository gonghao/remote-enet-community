package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/centerstate"
	"remote-enet/internal/identity"
	"remote-enet/internal/observe"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/settings"
	"remote-enet/internal/transport"
	relay "remote-enet/internal/transport/hub"
	"remote-enet/internal/trust"
)

// NewWithCenterHTTP uses the supplied TLS trust and transport for center requests, including account management.
func NewWithCenterHTTP(client *http.Client) *Runtime {
	r := New()
	r.centerHTTP = client
	return r
}

// CenterConfig selects the center and node for a reception. An empty Node resumes an existing reception.
type CenterConfig struct {
	Center     settings.Center
	Node       string
	AppVersion string
}

// CenterSnapshot contains only display metadata safe for subscriptions and diagnostics.
type CenterSnapshot struct {
	centerflow.State
	SASID       uint64
	SASDeadline time.Time
}

type centerReception interface {
	Snapshot() centerflow.State
	End(context.Context) error
}

type sasAnswer struct {
	ctx      context.Context
	prompt   centerflow.SASPrompt
	answer   chan bool
	verified bool
}

func (r *Runtime) centerSnapshotLocked() CenterSnapshot {
	s := r.state.Center
	if r.reception != nil {
		s.State = r.reception.Snapshot()
	}
	if p := r.pending; p != nil && p.ctx.Err() == nil {
		s.SASID, s.SASDeadline = p.prompt.ID, p.prompt.Deadline
	}
	return s
}

// CenterDisplay reads ephemeral secrets solely for the local invitation and confirmation controls.
func (r *Runtime) CenterDisplay() (invitation string, prompt centerflow.SASPrompt) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return
	}
	if op, ok := r.reception.(*centerflow.Operator); ok {
		invitation = op.Invitation()
	}
	if p := r.pending; p != nil && p.ctx.Err() == nil {
		prompt = p.prompt
	}
	return
}

func (r *Runtime) confirm(ctx context.Context, prompt centerflow.SASPrompt) (bool, error) {
	p := &sasAnswer{ctx: ctx, prompt: prompt, answer: make(chan bool, 1)}
	r.mu.Lock()
	r.pending = p
	r.publishLocked()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		if r.pending == p {
			r.pending = nil
			r.publishLocked()
		}
		r.mu.Unlock()
	}()
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case match := <-p.answer:
		return match, ctx.Err()
	}
}

// ErrSASExpired includes prompts that have already been answered.
var ErrSASExpired = errors.New("app: SAS prompt expired or no longer pending")

// ErrSASNotVerified leaves the operator prompt pending for another comparison.
var ErrSASNotVerified = errors.New("app: matching operator SAS input required")

func (p *sasAnswer) live(id uint64) bool {
	return p != nil && p.prompt.ID == id && p.ctx.Err() == nil && (p.prompt.Deadline.IsZero() || time.Now().Before(p.prompt.Deadline))
}

// VerifySAS binds the most recent comparison to this live operator prompt.
func (r *Runtime) VerifySAS(id uint64, input string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := r.pending
	if r.state.Role != session.RoleOperator || !p.live(id) {
		return false
	}
	digits := strings.Map(func(r rune) rune {
		switch {
		case r == ' ' || r == '　':
			return -1
		case r >= '０' && r <= '９':
			return '0' + r - '０'
		default:
			return r
		}
	}, input)
	p.verified = len(digits) == 6 && digits == p.prompt.Code
	return p.verified
}

// AnswerSAS consumes one live prompt. A failed verification leaves it pending.
func (r *Runtime) AnswerSAS(ctx context.Context, id uint64, match bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	p := r.pending
	if !p.live(id) {
		return ErrSASExpired
	}
	if match && r.state.Role == session.RoleOperator && !p.verified {
		return ErrSASNotVerified
	}
	r.pending = nil
	p.answer <- match
	r.publishLocked()
	return nil
}

// EndReception commits a user-requested stop to the center. Canceling Run alone permits recovery.
func (r *Runtime) EndReception(ctx context.Context) error {
	r.mu.Lock()
	for r.centerEnding != nil {
		ended := r.centerEnding
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ended:
		}
		r.mu.Lock()
	}
	if err := ctx.Err(); err != nil {
		r.mu.Unlock()
		return err
	}
	f := r.reception
	if f == nil {
		r.mu.Unlock()
		return ErrNotConnected
	}
	r.centerEnding = make(chan struct{})
	r.stopCenter()
	r.mu.Unlock()
	err := f.End(ctx)
	r.mu.Lock()
	close(r.centerEnding)
	r.centerEnding = nil
	r.publishLocked()
	r.mu.Unlock()
	return centerError(err)
}

// RetryPairing permits one additional exchange while the client is waiting for manual retry.
func (r *Runtime) RetryPairing(ctx context.Context) error {
	r.mu.Lock()
	cl, ok := r.reception.(*centerflow.Client)
	r.mu.Unlock()
	if !ok {
		return ErrNotConnected
	}
	return centerError(cl.Retry(ctx))
}

func receptionResult(f centerReception, err error) error {
	s := f.Snapshot()
	if err == nil && s.Phase == "ended" && s.Reason != "" && s.Reason != centerflow.OperatorEnded && s.Reason != centerflow.ClientEnded {
		return s.Reason
	}
	return err
}

// Keep signed request URLs (including full session IDs) out of CLI logs.
type centerFailure struct {
	PairingErrorKind
	cause error
}

func (e centerFailure) Unwrap() []error { return []error{e.PairingErrorKind, e.cause} }

func centerError(err error) error {
	if reason := centerflow.ReasonOf(err); reason != "" {
		return centerFailure{PairingErrorKind(reason), err}
	}
	return err
}

func centerAccountDeps(cfg settings.Center, version, role, dir string, id *identity.Local, hc *http.Client, logger *slog.Logger) (centerflow.Dependencies, error) {
	var d centerflow.Dependencies
	if cfg.ID == "" || len(cfg.API) == 0 {
		return d, centerflow.CenterUnconfigured
	}
	api, err := center.NewClient(center.ClientOptions{Center: cfg, Role: role, AppVersion: version, Device: id.Key, HTTP: hc, Logger: logger})
	if err != nil {
		return d, err
	}
	store, err := centerstate.Open(dir, cfg.ID)
	if err != nil {
		return d, err
	}
	return centerflow.Dependencies{API: api, Store: store, CenterID: cfg.ID, LocalFP: id.Fingerprint()}, nil
}

// CenterAccount opens the operator identity used by RunOperator for idle account management.
func (r *Runtime) CenterAccount(ctx context.Context, dir string, cfg CenterConfig) (*centerflow.Operator, error) {
	store, err := trust.Open(ctx, dir)
	if err != nil {
		return nil, err
	}
	id, err := store.LoadIdentity(ctx)
	if err != nil {
		return nil, err
	}
	d, err := centerAccountDeps(cfg.Center, cfg.AppVersion, "operator", dir, id, r.centerHTTP, nil)
	if err != nil {
		return nil, centerError(err)
	}
	return centerflow.NewOperator(d), nil
}

// Flow invokes connect and serve serially. Keep the transport alongside the released session for path and teardown.
type centerTransport struct {
	*relay.Connector
	current    transport.Session
	beforeDial func(context.Context) error
}

func (t *centerTransport) WaitTicket(ctx context.Context, ep transport.Endpoint, fp identity.Fingerprint, token []byte) (transport.Session, error) {
	s, err := t.Connector.WaitTicket(ctx, ep, fp, token)
	t.current = s
	return s, err
}
func (t *centerTransport) DialTicket(ctx context.Context, ep transport.Endpoint, fp identity.Fingerprint, token []byte) (transport.Session, error) {
	if err := t.beforeDial(ctx); err != nil {
		return nil, err
	}
	s, err := t.Connector.DialTicket(ctx, ep, fp, token)
	t.current = s
	return s, err
}

func (r *Runtime) centerDeps(cfg settings.Center, version, role, dir string, id *identity.Local, logger *slog.Logger) (centerflow.Dependencies, *centerTransport, error) {
	d, err := centerAccountDeps(cfg, version, role, dir, id, r.centerHTTP, logger)
	if err != nil {
		return d, nil, err
	}
	c, _, err := relay.New(relay.Options{Local: id, Wrap: wrap(id)})
	if err != nil {
		return d, nil, err
	}
	t := &centerTransport{Connector: c}
	d.Connector, d.Confirm = t, r.confirm
	r.update(func(s *Snapshot) { s.Device = id.Fingerprint(); s.Phase = PhaseConnecting })
	return d, t, nil
}

// centerflow exposes snapshots without a change channel. Notify subscribers only on transitions.
func (r *Runtime) watchReception(ctx context.Context, stopRun context.CancelFunc, f centerReception) func() {
	ctx, cancel := context.WithCancel(ctx)
	r.mu.Lock()
	r.reception = f
	r.stopCenter = stopRun
	r.publishLocked()
	r.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		var last centerflow.State
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if s := f.Snapshot(); s != last {
					last = s
					r.update(func(*Snapshot) {})
				}
			}
		}
	}()
	return func() { cancel(); <-done }
}

func (r *Runtime) runCenterOperator(ctx context.Context, cfg OperatorConfig, id *identity.Local) error {
	cc := cfg.Center
	var node center.NodeID
	if cc.Node != "" {
		if err := node.UnmarshalText([]byte(cc.Node)); err != nil {
			return fmt.Errorf("app: invalid center node ID: %w", err)
		}
	}
	d, t, err := r.centerDeps(cc.Center, cc.AppVersion, "operator", cfg.StateDir, id, cfg.Logger)
	if err != nil {
		return err
	}
	defer t.Close()
	op := centerflow.NewOperator(d)
	ctx, stopRun := context.WithCancel(ctx)
	defer stopRun()
	stop := r.watchReception(ctx, stopRun, op)
	defer stop()
	if cc.Node != "" {
		if err := op.Start(ctx, node); err != nil {
			return err
		}
	}
	states := make(vehicleStates, 1)
	var active atomic.Pointer[measurements]
	base := session.Options{Logger: cfg.Logger, AppVersion: cc.AppVersion, AllowLinkTest: cfg.AllowLinkTest, OnMessage: states.receive, OnRTT: func(d time.Duration) {
		if m := active.Load(); m != nil {
			m.onRTT(d)
		}
	}}
	err = op.Run(ctx, base, func(ctx context.Context, s *session.Session) (err error) {
		m := &measurements{runtime: r, direction: observe.Outbound}
		active.Store(m)
		defer func() {
			active.Store(nil)
			m.end(err)
			r.deactivate()
			select {
			case <-states:
			default:
			}
		}()
		fatal, err := r.presentVehicle(ctx, s, t.current.Path(), cfg, m, states)
		if !fatal && err != nil && ctx.Err() == nil {
			_ = t.current.Close(err)
			<-s.Done()
		}
		return err
	})
	return receptionResult(op, err)
}

func (r *Runtime) runCenterClient(parent context.Context, cfg ClientConfig, id *identity.Local) error {
	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	d, t, err := r.centerDeps(*cfg.Center, cfg.AppVersion, "client", cfg.StateDir, id, cfg.Logger)
	if err != nil {
		return err
	}
	defer t.Close()
	var nic carside.NIC
	var gw carside.Gateway
	t.beforeDial = func(ctx context.Context) error {
		delay := cfg.RetryMin
		for !gw.Address.IsValid() {
			var err error
			nic, gw, err = r.discoverVehicle(ctx, cfg, &delay)
			if err != nil {
				if ctx.Err() == nil {
					cancel(err)
				}
				return err
			}
		}
		return nil
	}
	checkNIC := func(ctx context.Context) error { _, err := selectNIC(ctx, cfg); return err }
	cl := centerflow.NewClient(centerflow.ClientDeps{Dependencies: d, CheckNIC: checkNIC})
	stop := r.watchReception(ctx, func() { cancel(context.Canceled) }, cl)
	defer stop()
	if cfg.CenterResume != (center.SessionID{}) {
		if err := checkNIC(ctx); err != nil {
			return errors.Join(centerflow.NICMissing, err)
		}
		if err := cl.Resume(ctx, cfg.CenterResume); err != nil {
			return err
		}
	} else if cfg.CenterCode != "" {
		if err := cl.Redeem(ctx, cfg.CenterCode); err != nil {
			return err
		}
		cfg.CenterCode = ""
	}
	var forwarder atomic.Pointer[carside.Forwarder]
	var active atomic.Pointer[measurements]
	base := session.Options{AppVersion: cfg.AppVersion, Logger: cfg.Logger, AllowLinkTest: cfg.AllowLinkTest, OnRTT: func(d time.Duration) {
		if m := active.Load(); m != nil {
			m.onRTT(d)
		}
	}, OnTunnel: func(ctx context.Context, h proto.OpenTunnel, st transport.Stream) {
		if f := forwarder.Load(); f != nil {
			f.HandleTunnel(ctx, h, st)
		}
	}}
	err = cl.Run(ctx, base, func(ctx context.Context, s *session.Session) (err error) {
		m := &measurements{runtime: r, direction: observe.Inbound}
		fresh, err := selectNIC(ctx, cfg)
		if err != nil || fresh != nic {
			gw = carside.Gateway{}
			_ = t.current.Close(ErrNICChanged)
			<-s.Done()
			return ErrNICChanged
		}
		f, err := carside.NewForwarder(nic, cfg.Logger, carside.WithObserver(m))
		if err != nil {
			return err
		}
		forwarder.Store(f)
		active.Store(m)
		m.start()
		defer func() {
			forwarder.Store(nil)
			active.Store(nil)
			f.Close()
			m.end(err)
			r.deactivate()
			gw = carside.Gateway{}
		}()
		if err := s.Send(gw.VehicleState(nic, 1)); err != nil {
			return err
		}
		r.activate(ctx, s, t.current.Path())
		if err := r.monitorNIC(ctx, nic, cfg); err != nil {
			_ = t.current.Close(err)
			<-s.Done()
			return err
		}
		return ctx.Err()
	})
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	return receptionResult(cl, err)
}
