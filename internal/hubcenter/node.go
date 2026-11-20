package hubcenter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"slices"
	"sync/atomic"
	"time"

	"remote-enet/internal/center"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
)

// ErrSuperseded requires an operator restart to acquire a new generation.
var ErrSuperseded = errors.New("node generation superseded; another process may be using this identity")

// NodeOptions binds the same node identity, policy and clock used by Server and API.
type NodeOptions struct {
	Config  Config
	Local   *identity.Local
	Server  *hub.Server
	Policy  *Policy
	API     *center.Client
	Version string
	Now     func() time.Time
	Logger  *slog.Logger
}

// Node owns one activation and its serial stream of heartbeats.
type Node struct {
	started atomic.Bool
	o       NodeOptions
	wait    func(context.Context, time.Duration) error
}

const number = `(?:0|[1-9][0-9]*)`
const preID = `(?:` + number + `|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`

var versionPattern = regexp.MustCompile(`^` + number + `\.` + number + `\.` + number + `(?:-` + preID + `(?:\.` + preID + `)*)?(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// NewNode closes admission before the caller starts the Hub listener.
func NewNode(o NodeOptions) *Node {
	o.Server.SetAdmitting(false)
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if !versionPattern.MatchString(o.Version) {
		o.Version = "0.0.0-dev"
	}
	o.Config.Center.API = slices.Clone(o.Config.Center.API)
	return &Node{o: o, wait: waitContext}
}

func waitContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

// Reload atomically publishes validated ticket keys. Other changes require restart.
func (n *Node) Reload(cfg Config) error {
	if err := cfg.ReloadCompatible(n.o.Config); err != nil {
		return err
	}
	return n.o.Policy.SetKeys(cfg.TicketKeys)
}

func (n *Node) kids() []center.KeyID {
	keys := n.o.Policy.Kids()
	ids := make([]center.KeyID, len(keys))
	for i, k := range keys {
		ids[i] = center.KeyID(k)
	}
	return ids
}

func (n *Node) closeAll() {
	n.o.Server.SetAdmitting(false)
	n.o.Server.CloseAll()
}

func (n *Node) clockOK(ctx context.Context, server, sent, received time.Time) bool {
	half := received.Sub(sent) / 2
	offset := server.Sub(sent.Add(half))
	if half < 0 || offset.Abs() > ClockSkew-time.Second-half {
		n.o.Logger.ErrorContext(ctx, "Node clock skew exceeds tolerance", "offset", offset, "half_rtt", half)
		return false
	}
	return true
}

func (n *Node) checkPolicy(ctx context.Context, p center.NodePolicy, sent, received time.Time) bool {
	retention := min(seconds(p.LeaseTTLS), time.Duration(math.MaxInt64)-ClockSkew) + ClockSkew
	n.o.Server.SetSessionRetention(retention)
	ok := n.clockOK(ctx, time.Unix(p.ServerTime, 0), sent, received)
	if p.NodeState == "disabled" {
		n.closeAll()
		return false
	}
	if !ok {
		n.o.Server.SetAdmitting(false)
	}
	return ok
}

func errorCode(err error) string {
	var api *center.Error
	if errors.As(err, &api) {
		return api.Code
	}
	return ""
}

func (n *Node) skewError(ctx context.Context, err error, sent, received time.Time) {
	n.o.Server.SetAdmitting(false)
	var api *center.Error
	if errors.As(err, &api) {
		if ts, ok := api.ServerTime(); ok {
			n.clockOK(ctx, ts, sent, received)
		}
	}
}

func (n *Node) activate(ctx context.Context) (center.Activated, error) {
	var key string
	var created time.Time
	var body center.Activation
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return center.Activated{}, err
		}
		sent := n.o.Now()
		if key == "" || sent.Sub(created) >= 23*time.Hour {
			key, created = center.NewIdempotencyKey(), sent
			body = center.Activation{Version: n.o.Version, Kids: n.kids()}
		}
		a, err := n.o.API.Activate(ctx, key, body)
		received := n.o.Now()
		if ctx.Err() != nil {
			return center.Activated{}, ctx.Err()
		}
		if err == nil {
			n.o.Policy.SetGeneration(a.Gen)
			if n.checkPolicy(ctx, a.NodePolicy, sent, received) {
				n.o.Server.SetAdmitting(true)
			}
			n.o.Logger.InfoContext(ctx, "Node activated", "hub", n.o.Local.Fingerprint(), "center", n.o.Config.Center.ID, "node", center.NodeID(n.o.Config.NodeID).String()[:8], "gen", a.Gen, "listen", n.o.Config.Listen)
			return a, nil
		}
		n.o.Logger.ErrorContext(ctx, "Node activation failed", "error", err)
		delay := backoff
		switch errorCode(err) {
		case "node_superseded":
			return center.Activated{}, ErrSuperseded
		case "clock_skew":
			n.skewError(ctx, err, sent, received)
			delay = 15 * time.Second
		default:
			if !errors.Is(err, center.ErrUnreachable) && !errors.Is(err, context.DeadlineExceeded) {
				return center.Activated{}, fmt.Errorf("activate node: %w", err)
			}
			backoff = min(2*backoff, 30*time.Second)
		}
		if err := n.wait(ctx, delay); err != nil {
			return center.Activated{}, err
		}
	}
}

// Run must be called once, after Serve starts. Cancellation closes admission and routes.
func (n *Node) Run(ctx context.Context) (err error) {
	if !n.started.CompareAndSwap(false, true) {
		return errors.New("node already started")
	}
	defer func() {
		n.closeAll()
		if errors.Is(err, ErrSuperseded) {
			n.o.Logger.ErrorContext(ctx, "Node generation superseded; another process may be using this identity")
		}
	}()
	a, err := n.activate(ctx)
	if err != nil {
		return err
	}
	last := n.sample()
	interval := seconds(a.HeartbeatIntervalS)
	next := last.at.Add(interval)
	var protocolErrors uint64
	var closed []center.ClosedUsage
	for seq := uint64(1); ; seq++ {
		if err := n.wait(ctx, next.Sub(n.o.Now())); err != nil {
			return err
		}
		current := n.sample()
		req := n.report(ctx, a.Gen, seq, last, current, closed)
		last = current
		attempt, cancel := context.WithTimeout(ctx, HeartbeatTimeout)
		sent := n.o.Now()
		reply, err := n.o.API.Heartbeat(attempt, req)
		received := n.o.Now()
		if attempt.Err() != nil || received.Sub(sent) > HeartbeatTimeout {
			err = context.DeadlineExceeded
		}
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			if errors.Is(err, center.ErrEndpointResponse) {
				protocolErrors++
				n.o.Logger.ErrorContext(ctx, "Invalid heartbeat response", "seq", seq, "protocol_errors", protocolErrors, "error", err)
			} else {
				n.o.Logger.ErrorContext(ctx, "Node heartbeat failed", "seq", seq, "error", err)
			}
			switch errorCode(err) {
			case "node_superseded":
				return ErrSuperseded
			case "node_disabled", "unauthenticated", "upgrade_required":
				n.closeAll()
			case "clock_skew":
				n.skewError(ctx, err, sent, received)
			}
		} else {
			admit := n.checkPolicy(ctx, reply.NodePolicy, sent, received)
			reported := make(map[center.SessionID]bool, len(req.Sessions))
			for _, u := range req.Sessions {
				reported[u.SessionID] = true
			}
			for _, r := range reply.Renew {
				if !reported[r.SessionID] || !n.o.Server.Renew(hub.SessionID(r.SessionID), sent.Add(seconds(r.LeaseS))) {
					n.o.Logger.WarnContext(ctx, "Ignoring renewal for unknown session", "seq", seq, "session", r.SessionID.String())
				}
			}
			closed = n.revoke(reply.Revoke, closed)
			if admit {
				n.o.Server.SetAdmitting(true)
			}
			interval = seconds(reply.HeartbeatIntervalS)
		}
		next = sent.Add(interval)
	}
}

func seconds(s int64) time.Duration {
	return time.Duration(min(s, math.MaxInt64/int64(time.Second))) * time.Second
}
