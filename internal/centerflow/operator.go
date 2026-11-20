package centerflow

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/centercode"
	"remote-enet/internal/centerstate"
	"remote-enet/internal/session"
)

// Operator owns a reception for one enrolled identity.
type Operator struct{ *flow }

// NewOperator leaves account reconciliation to Run or Start.
func NewOperator(d OperatorDeps) *Operator { return &Operator{newFlow(d, session.RoleOperator)} }

// Login persists display metadata only after device enrollment succeeds.
func (o *Operator) Login(ctx context.Context, code, label string) error {
	if err := o.lockCommand(ctx); err != nil {
		return err
	}
	defer o.unlockCommand()
	code, err := centercode.NormalizeAccountCode(code)
	if err != nil {
		return o.fail(ctx, errors.Join(Reason("account_code_format"), err))
	}
	d, a, err := o.API.Login(ctx, center.NewIdempotencyKey(), code, label)
	if err != nil {
		return o.fail(ctx, err)
	}
	return o.Store.SaveLogin(ctx, centerstate.Login{DeviceFP: d.DeviceFP, DeviceLabel: d.Label, AccountID: a.AccountID.String(), AccountLabel: a.Label, ExpiresAt: time.Unix(d.ExpiresAt, 0), LoggedInAt: time.Now()})
}

// Logout revokes the signing device even when its display cache is absent.
func (o *Operator) Logout(ctx context.Context) error {
	if err := o.lockCommand(ctx); err != nil {
		return err
	}
	defer o.unlockCommand()
	if err := o.stop(ctx); err != nil {
		return err
	}
	if err := o.API.RevokeDevice(ctx, center.NewIdempotencyKey(), o.LocalFP); err != nil {
		return o.fail(ctx, err)
	}
	var err error
	if o.current != nil {
		err = o.cleanup(ctx, o.current, "device_revoked")
	}
	return errors.Join(err, o.Store.ClearLogin(ctx))
}

// Account reads authoritative occupancy, including sessions of other devices.
func (o *Operator) Account(ctx context.Context) (center.AccountView, error) {
	a, err := o.API.Account(ctx)
	if err != nil {
		return a, o.fail(ctx, err)
	}
	return a, nil
}

// NodeLatency is negative when the TCP probe fails.
type NodeLatency struct {
	center.Node
	Latency time.Duration
}

// Nodes probes up to four TCP listeners concurrently without a TLS handshake.
func (o *Operator) Nodes(ctx context.Context) ([]NodeLatency, error) {
	nodes, err := o.API.Nodes(ctx)
	if err != nil {
		return nil, o.fail(ctx, err)
	}
	out := make([]NodeLatency, len(nodes))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range min(4, len(nodes)) {
		wg.Go(func() {
			d := net.Dialer{Timeout: 3 * time.Second}
			for i := range jobs {
				n := nodes[i]
				out[i] = NodeLatency{n, -1}
				start := time.Now()
				c, err := d.DialContext(ctx, "tcp", addr(n))
				if err == nil {
					out[i].Latency = time.Since(start)
					c.Close()
				}
			}
		})
	}
	for i := range nodes {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	return out, ctx.Err()
}

// Invitation is for the local code display only. Snapshot is safe for diagnostics.
func (o *Operator) Invitation() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.current != nil && o.state.Phase == "open" && time.Now().Before(o.state.CodeExpiresAt) {
		return o.current.code
	}
	return ""
}
func (o *Operator) resume(ctx context.Context) error {
	a, err := o.Account(ctx)
	if err != nil {
		return err
	}
	if err := o.prune(ctx); err != nil {
		return err
	}
	for _, item := range a.Sessions {
		if item.OperatorFP == o.LocalFP {
			s, err := o.API.Session(ctx, item.SessionID, 0, 0, false)
			if err != nil {
				return o.fail(ctx, err)
			}
			return o.recover(ctx, s)
		}
	}
	return nil
}

// Start creates one reception or reconciles the signing device's existing session.
func (o *Operator) Start(parent context.Context, node center.NodeID) error {
	ctx, cancelBudget := context.WithTimeoutCause(parent, o.operationBudget, center.ErrUnreachable)
	defer cancelBudget()
	if err := o.lockCommand(ctx); err != nil {
		return err
	}
	if o.occupied() {
		o.unlockCommand()
		return DeviceBusy
	}
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	o.mu.Lock()
	o.cancel, o.done = cancel, done
	o.mu.Unlock()
	o.unlockCommand()
	defer func() {
		cancel()
		o.mu.Lock()
		o.cancel, o.done = nil, nil
		close(done)
		o.mu.Unlock()
	}()
	err := o.create(ctx, node)
	if err != nil && context.Cause(ctx) != nil {
		return o.fail(ctx, context.Cause(ctx))
	}
	return err
}

func (o *Operator) create(ctx context.Context, node center.NodeID) error {
	key := center.NewIdempotencyKey()
	s, err := retryAPI(ctx, o.flow, func() (center.Session, error) { return o.API.CreateSession(ctx, key, node) })
	if ReasonOf(err) == DeviceBusy {
		if resumeErr := o.resume(ctx); resumeErr != nil {
			return resumeErr
		}
		if o.current == nil {
			return o.fail(ctx, err)
		}
		return nil
	}
	if err != nil {
		return o.fail(ctx, err)
	}
	if s.Code == nil {
		return o.recover(ctx, s)
	}
	o.install(s, *s.Code)
	return nil
}
func (f *flow) occupied() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.current != nil || f.done != nil
}

// Run reconciles the account first if Start has not installed a reception.
func (o *Operator) Run(ctx context.Context, base session.Options, serve func(context.Context, *session.Session) error) error {
	return o.run(ctx, base, serve, o.resume)
}
