// Package centerflow coordinates one device's reception at one configured center.
package centerflow

import (
	"context"
	"errors"

	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/centerstate"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
)

// Reason is a stable UI key. Center business codes and end reasons retain their wire spelling.
type Reason string

func (r Reason) Error() string { return string(r) }

const (
	PeerVersionIncompatible Reason = "peer_version_incompatible"
	CenterProtocolError     Reason = "center_protocol_error"
	NICMissing              Reason = "nic_missing"
	CenterUnconfigured      Reason = "center_unconfigured"
	CenterUnreachable       Reason = "center_unreachable"
	DeviceBusy              Reason = "device_busy"
	CodeInvalid             Reason = "code_invalid"
	PairingLost             Reason = "pairing_lost"
	HubUnauthorized         Reason = "hub_unauthorized"
	OperatorWaiting         Reason = "operator_waiting"
	Busy                    Reason = "busy"
	Confirmation            Reason = "confirmation"
	CenterIDMismatch        Reason = "center_id_mismatch"
	UpgradeRequired         Reason = "upgrade_required"
	RateLimited             Reason = "rate_limited"
	AccountCodeFormat       Reason = "account_code_format"
	AccountCodeInvalid      Reason = "account_code_invalid"
	AccountDisabled         Reason = "account_disabled"
	AccountExpired          Reason = "account_expired"
	DeviceLimit             Reason = "device_limit"
	Unauthenticated         Reason = "unauthenticated"
	QuotaExhausted          Reason = "quota_exhausted"
	QuotaReleasing          Reason = "quota_releasing"
	NodeUnavailable         Reason = "node_unavailable"
	AwaitingCenter          Reason = "awaiting_center"
	OperatorEnded           Reason = "operator_ended"
	ClientEnded             Reason = "client_ended"
	Rejected                Reason = "rejected"
	ConfirmTimeout          Reason = "confirm_timeout"
	PakeFailed              Reason = "pake_failed"
	CodeExpired             Reason = "code_expired"
	RedeemLimit             Reason = "redeem_limit"
	PairTimeout             Reason = "pair_timeout"
	IdleTimeout             Reason = "idle_timeout"
	DeviceRevoked           Reason = "device_revoked"
	NodeLost                Reason = "node_lost"
	NodeDisabled            Reason = "node_disabled"
	AdminRevoked            Reason = "admin_revoked"
	NotFound                Reason = "not_found"
	Forbidden               Reason = "forbidden"
)

// ReasonOf ignores server message text and preserves the authoritative end reason.
func ReasonOf(err error) Reason {
	var r Reason
	var e *center.Error
	switch {
	case errors.As(err, &r):
		return r
	case errors.As(err, &e) && e.Code == "session_ended" && e.EndReason() != "":
		return Reason(e.EndReason())
	case errors.Is(err, session.ReasonUnsupported):
		return "unsupported"
	case errors.Is(err, session.ErrVersion):
		return PeerVersionIncompatible
	case errors.Is(err, center.ErrProtocol):
		return CenterProtocolError
	case errors.Is(err, center.ErrBadRequest):
		return "bad_request"
	case errors.Is(err, center.ErrUnreachable):
		return CenterUnreachable
	case errors.As(err, &e) && e.Code != "":
		return Reason(e.Code)
	default:
		return ""
	}
}

// SASPrompt is ephemeral. An operator UI may confirm only after matching the customer's six digits.
type SASPrompt struct {
	ID       uint64
	Code     string
	Peer     identity.Fingerprint
	Deadline time.Time
}

// Confirm must release its dialog and return when ctx ends.
type Confirm func(context.Context, SASPrompt) (bool, error)

// TicketConnector performs a single authenticated outer and inner connection.
type TicketConnector interface {
	WaitTicket(context.Context, transport.Endpoint, identity.Fingerprint, []byte) (transport.Session, error)
	DialTicket(context.Context, transport.Endpoint, identity.Fingerprint, []byte) (transport.Session, error)
}

// Dependencies must describe the same role, local identity and center.
type Dependencies struct {
	API                *center.Client
	CenterID           string
	LocalFP            identity.Fingerprint
	Store              *centerstate.Store
	Connector          TicketConnector
	Confirm            Confirm
	RetryMin, RetryMax time.Duration
}

// OperatorDeps uses the signer enrolled by Login.
type OperatorDeps = Dependencies

// ClientDeps checks the selected adapter before redemption.
type ClientDeps struct {
	Dependencies
	// CheckNIC must succeed before a single-use code may be redeemed.
	CheckNIC func(context.Context) error
}

// State omits codes, tickets and full session IDs. Times are zero when unavailable.
type State struct {
	Phase                    string
	Reason                   Reason
	SessionShort             string
	Region                   string
	PeerShort                string
	AttemptsLeft             int
	CodeExpiresAt, ExpiresAt time.Time
}

// OperatorState exposes invitation expiry while the code is visible.
type OperatorState = State

// ClientState reports the remaining manual pairing attempts.
type ClientState = State

type flow struct {
	Dependencies
	role               session.Role
	operationBudget    time.Duration
	unreachableRetries int
	// Commands serialize replacement, cancellation and disposal of the current reception.
	cmd     chan struct{}
	mu      sync.Mutex
	state   State
	current *reception
	cancel  context.CancelFunc
	done    chan struct{}
}
type reception struct {
	receivedTicket bool
	remote         center.Session
	code           string
	responder      *pairing.SessionResponder
	verified       identity.Fingerprint
	left           int
	retry          chan struct{}
	establishedKey string
	pendingEnd     Reason
	endKey         string
}

var promptID atomic.Uint64

func newFlow(d Dependencies, role session.Role) *flow {
	if d.RetryMin <= 0 {
		d.RetryMin = 500 * time.Millisecond
	}
	if d.RetryMax <= 0 {
		d.RetryMax = 5 * time.Second
	}
	d.RetryMax = max(d.RetryMin, d.RetryMax)
	return &flow{operationBudget: time.Minute, cmd: make(chan struct{}, 1), Dependencies: d, role: role, state: State{Phase: "idle"}}
}

func (f *flow) lockCommand(ctx context.Context) error {
	select {
	case f.cmd <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (f *flow) unlockCommand() { <-f.cmd }

// Snapshot may be read while Run and the confirmation dialog are active.
func (f *flow) Snapshot() State {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.state
	if f.unreachableRetries > 0 {
		s.Reason = CenterUnreachable
	}
	return s
}
func (f *flow) phase(p string, r Reason) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.state.Phase = p
	f.state.Reason = r
}
func (f *flow) fail(ctx context.Context, err error) error {
	r := ReasonOf(err)
	if r == "unauthenticated" && f.role == session.RoleOperator {
		err = errors.Join(err, f.Store.ClearLogin(ctx))
	}
	f.mu.Lock()
	f.state.Reason = r
	f.mu.Unlock()
	return err
}
func addr(n center.Node) string { return net.JoinHostPort(n.Addr, strconv.Itoa(int(n.Port))) }
func timestamp(p *int64) time.Time {
	if p == nil {
		return time.Time{}
	}
	return time.Unix(*p, 0)
}
func (f *flow) install(s center.Session, code string) *reception {
	s.Code = nil
	r := &reception{remote: s, code: code, left: 3, establishedKey: center.NewIdempotencyKey()}
	if f.role == session.RoleOperator && code != "" {
		r.responder = pairing.NewSessionResponder(code, 3)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.current = r
	f.state = State{Phase: s.State, SessionShort: s.SessionID.String()[:6], Region: s.Node.Region, CodeExpiresAt: timestamp(s.CodeExpiresAt), ExpiresAt: timestamp(s.ExpiresAt), AttemptsLeft: 3}
	return r
}
func (f *flow) cleanup(ctx context.Context, r *reception, why Reason) error {
	if r.responder != nil {
		r.responder.Close()
	}
	f.mu.Lock()
	r.code = ""
	r.verified = identity.Fingerprint{}
	f.state.Phase = "ended"
	f.state.Reason = why
	f.state.CodeExpiresAt = time.Time{}
	f.mu.Unlock()
	if err := f.Store.DeletePin(ctx, r.remote.SessionID); err != nil {
		return err
	}
	f.mu.Lock()
	f.current = nil
	f.mu.Unlock()
	return nil
}
func (f *flow) end(ctx context.Context, r *reception, why Reason) error {
	if r.pendingEnd == "" {
		r.pendingEnd = why
		r.endKey = center.NewIdempotencyKey()
	}
	key := r.endKey
	s, err := retryAPI(ctx, f, func() (center.Session, error) { return f.API.End(ctx, key, r.remote.SessionID, string(r.pendingEnd)) })
	if err != nil {
		var api *center.Error
		if errors.As(err, &api) && (api.Code == "not_found" || api.Code == "session_ended") {
			return f.cleanup(ctx, r, ReasonOf(err))
		}
		return f.handle(ctx, r, err)
	}
	if s.EndReason == nil {
		return center.ErrProtocol
	}
	return f.cleanup(ctx, r, Reason(*s.EndReason))
}
func (f *flow) handle(ctx context.Context, r *reception, err error) error {
	var api *center.Error
	if errors.As(err, &api) && (api.Code == "session_ended" || api.Code == "not_found" || api.Code == "unauthenticated") {
		return errors.Join(f.fail(ctx, err), f.cleanup(ctx, r, ReasonOf(err)))
	}
	return f.fail(ctx, err)
}
func (f *flow) stop(ctx context.Context) error {
	f.mu.Lock()
	cancel, done := f.cancel, f.done
	f.mu.Unlock()
	if cancel != nil {
		cancel()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// End stops local work before ending the reception and removing its pin.
func (f *flow) End(ctx context.Context) error {
	if err := f.lockCommand(ctx); err != nil {
		return err
	}
	defer f.unlockCommand()
	if err := f.stop(ctx); err != nil {
		return err
	}
	if f.current == nil {
		return nil
	}
	return f.end(ctx, f.current, Reason(string(f.role)+"_ended"))
}
func (f *flow) recover(ctx context.Context, s center.Session) error {
	if s.State == "ended" {
		return f.cleanup(ctx, f.install(s, ""), Reason(*s.EndReason))
	}
	pin, err := f.Store.Pin(s.SessionID)
	if err != nil && !errors.Is(err, centerstate.ErrNoPin) && !errors.Is(err, centerstate.ErrCorrupt) {
		return err
	}
	r := f.install(s, "")
	if s.State == "open" || err != nil || !f.matches(pin, s) {
		return errors.Join(PairingLost, f.end(ctx, r, PairingLost))
	}
	r.verified = pin.Peer
	return nil
}
func (f *flow) peer(s center.Session) identity.Fingerprint {
	if f.role == session.RoleOperator {
		return *s.ClientFP
	}
	return s.OperatorFP
}
func (f *flow) matches(p centerstate.Pin, s center.Session) bool {
	return p.Role == string(f.role) && p.Peer == f.peer(s) && p.Node.ID == s.Node.NodeID && p.Node.HubFP == s.Node.HubFP && p.CenterID == f.CenterID
}
func (f *flow) save(ctx context.Context, s center.Session, peer identity.Fingerprint) error {
	return f.Store.SavePin(ctx, centerstate.Pin{CenterID: f.CenterID, Session: s.SessionID, Role: string(f.role), Peer: peer, PairedAt: time.Now(), Node: centerstate.NodeRef{ID: s.Node.NodeID, Addr: s.Node.Addr, Port: s.Node.Port, HubFP: s.Node.HubFP}})
}

// prune verifies each leftover pin directly, including sessions no longer visible in lists.
func (f *flow) prune(ctx context.Context) error {
	pins, err := f.Store.Pins()
	if err != nil && !errors.Is(err, centerstate.ErrCorrupt) {
		return err
	}
	for _, p := range pins {
		s, err := f.API.Session(ctx, p.Session, 0, 0, false)
		if err != nil && ReasonOf(err) != "not_found" {
			return err
		}
		if err != nil || s.State == "ended" {
			if err := f.Store.DeletePin(ctx, p.Session); err != nil {
				return err
			}
		}
	}
	return nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func retryAPI[T any](ctx context.Context, f *flow, call func() (T, error)) (T, error) {
	// A poll and a ticket request can retry concurrently. Keep the outage visible
	// until every affected call has returned.
	unreachable := false
	defer func() {
		if unreachable {
			f.mu.Lock()
			f.unreachableRetries--
			f.mu.Unlock()
		}
	}()
	for delay := f.RetryMin; ; delay = min(delay*2, f.RetryMax) {
		v, err := call()
		var api *center.Error
		wait := delay
		if errors.As(err, &api) && api.Class == center.ClassWait {
			if api.RetryAfter < 0 || api.Code == "session_not_ready" {
				return v, err
			}
			wait = max(delay, api.RetryAfter)
			f.mu.Lock()
			f.state.Reason = Reason(api.Code)
			f.mu.Unlock()
		} else if errors.Is(err, center.ErrUnreachable) {
			if !unreachable {
				f.mu.Lock()
				f.unreachableRetries++
				f.mu.Unlock()
				unreachable = true
			}
		} else {
			return v, err
		}
		if err := sleep(ctx, wait); err != nil {
			return v, err
		}
	}
}

var errRunning = errors.New("centerflow: reception already running")
