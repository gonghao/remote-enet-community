package carside

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"

	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunnel"
)

// Forwarder owns handler lifetimes. It never interprets tunneled payload bytes.
type Forwarder struct {
	nic      NIC
	logger   *slog.Logger
	observer tunnel.Observer
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	closed   bool
	wg       sync.WaitGroup
}

// ForwarderOption configures a forwarder before any handlers are admitted.
type ForwarderOption func(*Forwarder)

// WithObserver observes opaque forwarding without affecting target selection.
func WithObserver(observer tunnel.Observer) ForwarderOption {
	return func(f *Forwarder) { f.observer = observer }
}

// NewForwarder fixes the source address and subnet for its entire lifetime.
func NewForwarder(nic NIC, logger *slog.Logger, options ...ForwarderOption) (*Forwarder, error) {
	if err := validateNIC(nic); err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &Forwarder{nic: nic, logger: logger, ctx: ctx, cancel: cancel}
	for _, option := range options {
		option(f)
	}
	return f, nil
}

// HandleTunnel takes ownership of st, including on rejected targets.
func (f *Forwarder) HandleTunnel(ctx context.Context, h proto.OpenTunnel, st transport.Stream) {
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		_ = st.Close()
		return
	}
	f.wg.Add(1)
	f.mu.Unlock()
	defer f.wg.Done()
	defer st.Close()
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(f.ctx, cancel)
	defer stop()
	defer cancel()
	if ctx.Err() != nil || f.ctx.Err() != nil {
		return
	}
	var activity tunnel.Activity
	if f.observer != nil {
		activity = f.observer.Begin(h.Target)
	}
	var err error
	defer func() {
		if activity != nil {
			activity.End(err)
		}
	}()
	target, err := f.target(h)
	if err != nil {
		f.logger.WarnContext(ctx, "Tunnel target rejected", "target", h.Target, "error", err)
		return
	}
	dialer := net.Dialer{LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(f.nic.Address, 0)), Timeout: 10 * time.Second,
		ControlContext: func(ctx context.Context, _, _ string, raw syscall.RawConn) error {
			return bindVehicleInterface(ctx, f.nic, raw)
		},
	}
	conn, err := dialer.DialContext(ctx, "tcp4", target.String())
	if err != nil {
		f.logger.WarnContext(ctx, "Vehicle connection failed", "target", target.String(), "error", err)
		return
	}
	var stats *tunnel.Stats
	if activity != nil {
		stats = &tunnel.Stats{Counter: activity}
	}
	err = tunnel.Pipe(ctx, st, conn, stats)
	if err != nil && ctx.Err() == nil {
		f.logger.DebugContext(ctx, "Tunnel closed", "target", target.String(), "error", err)
	}
}

func (f *Forwarder) target(h proto.OpenTunnel) (netip.AddrPort, error) {
	if h.Kind != "tcp" {
		return netip.AddrPort{}, fmt.Errorf("carside: unsupported tunnel kind %q", h.Kind)
	}
	target, err := netip.ParseAddrPort(h.Target)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("carside: target must be a literal IPv4 address and port: %w", err)
	}
	ip := target.Addr()
	if !ip.Is4() || target.Port() == 0 || !f.nic.Prefix.Contains(ip) || !ip.IsGlobalUnicast() && !ip.IsLinkLocalUnicast() && !ip.IsLoopback() || broadcast(ip, f.nic.Prefix) || network(ip, f.nic.Prefix) || ip.IsLoopback() && !f.nic.Address.IsLoopback() {
		return netip.AddrPort{}, fmt.Errorf("carside: target %s is not a permitted subnet host", target)
	}
	addresses, err := localIPv4Addresses()
	if err != nil {
		return netip.AddrPort{}, err
	}
	if _, local := addresses[ip]; local || ip == f.nic.Address {
		return netip.AddrPort{}, fmt.Errorf("carside: target %s is a local address", target)
	}
	return target, nil
}

// Close cancels and joins every admitted handler; subsequent handlers are rejected.
func (f *Forwarder) Close() error {
	f.mu.Lock()
	f.closed = true
	f.cancel()
	f.mu.Unlock()
	f.wg.Wait()
	return nil
}
