// Package operator presents one remote gateway on local HSFZ endpoints.
package operator

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunnel"
	"remote-enet/internal/udpdiscovery"
)

// Opener retains ownership of the session; each returned stream belongs to its caller.
type Opener interface {
	OpenTunnel(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error)
}

// Ports uses zero for an OS-assigned port on each independent listener.
type Ports struct{ Diagnostic, Control, Discovery int }

// Options requires an explicit IPv4 bind address. Remote zero ports use HSFZ defaults.
type Options struct {
	Bind                 netip.Addr
	Ports                Ports
	State                proto.VehicleState
	RemoteDiagnosticPort uint16
	RemoteControlPort    uint16
	Opener               Opener
	Logger               *slog.Logger
	// Exclusive is opt-in; Resolver is ignored when it is false.
	Exclusive bool
	Resolver  exclusive.Resolver
	// Gate shares ownership with other presentation paths; ignored unless Exclusive.
	Gate *exclusive.Gate
	// Capture is optional; its lifecycle belongs to the caller.
	Capture *hsfzcapture.Recorder
	// CaptureGeneration must return promptly and be concurrency-safe. It runs
	// outside presenter locks; nil records generation zero.
	CaptureGeneration func() uint64
	Observer          tunnel.Observer
}

type vehicle struct {
	address netip.Addr
	raw     []byte
}

// Presenter captures vehicle state per accepted TCP connection. Updates never
// retarget an existing stream, and Close never closes the upstream session.
type Presenter struct {
	ctx               context.Context
	cancel            context.CancelFunc
	opener            Opener
	gate              *exclusive.Gate
	capture           *hsfzcapture.Recorder
	captureGeneration func() uint64
	observer          tunnel.Observer
	logger            *slog.Logger
	diagPort          uint16
	ctrlPort          uint16
	diag, ctrl        *net.TCPListener
	disc              *udpdiscovery.Conn
	mu                sync.Mutex
	state             *vehicle
	connections       map[*net.TCPConn]struct{}
	wg                sync.WaitGroup
	once              sync.Once
	maintenance       bool
}

// Start acquires all three sockets before exposing any endpoint to workers.
// Its context controls the full presenter lifetime, including returned tunnels.
func Start(ctx context.Context, o Options) (*Presenter, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("operator: start: %w", err)
	}
	if !o.Bind.Is4() || o.Bind.IsUnspecified() || o.Bind.IsMulticast() || o.Bind == netip.MustParseAddr("255.255.255.255") {
		return nil, fmt.Errorf("operator: bind must be a specific unicast IPv4 address")
	}
	for _, port := range []int{o.Ports.Diagnostic, o.Ports.Control, o.Ports.Discovery} {
		if port < 0 || port > 65535 {
			return nil, fmt.Errorf("operator: local port %d outside 0..65535", port)
		}
	}
	if o.Opener == nil {
		return nil, fmt.Errorf("operator: tunnel opener required")
	}
	state, err := parseState(o.State)
	if err != nil {
		return nil, err
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.RemoteDiagnosticPort == 0 {
		o.RemoteDiagnosticPort = 6801
	}
	if o.RemoteControlPort == 0 {
		o.RemoteControlPort = 6811
	}
	runCtx, cancel := context.WithCancel(ctx)
	p := &Presenter{ctx: runCtx, cancel: cancel, opener: o.Opener, observer: o.Observer, capture: o.Capture, captureGeneration: o.CaptureGeneration, logger: o.Logger, state: state,
		diagPort: o.RemoteDiagnosticPort, ctrlPort: o.RemoteControlPort, connections: make(map[*net.TCPConn]struct{})}
	if o.Exclusive {
		p.gate = o.Gate
		if p.gate == nil {
			p.gate = exclusive.NewGate(o.Resolver)
		}
	}
	addr := func(port int) *net.TCPAddr {
		return net.TCPAddrFromAddrPort(netip.AddrPortFrom(o.Bind, uint16(port)))
	}
	p.diag, err = net.ListenTCP("tcp4", addr(o.Ports.Diagnostic))
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("operator: listen diagnostic: %w", err)
	}
	p.ctrl, err = net.ListenTCP("tcp4", addr(o.Ports.Control))
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("operator: listen control: %w", err)
	}
	p.disc, err = udpdiscovery.Listen(runCtx, o.Bind, o.Ports.Discovery)
	if err != nil {
		p.Close()
		return nil, fmt.Errorf("operator: listen discovery: %w", err)
	}
	p.wg.Add(3)
	go p.accept(p.diag, p.diagPort, "diagnostic")
	go p.accept(p.ctrl, p.ctrlPort, "control")
	go p.discover()
	context.AfterFunc(runCtx, func() { _ = p.Close() })
	if err := ctx.Err(); err != nil {
		p.Close()
		return nil, fmt.Errorf("operator: start: %w", err)
	}
	return p, nil
}

func parseState(s proto.VehicleState) (*vehicle, error) {
	if s.GatewayAddr == "" && len(s.IdentRaw) == 0 {
		return nil, nil
	}
	ip, err := netip.ParseAddr(s.GatewayAddr)
	if err != nil || !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() || ip == netip.MustParseAddr("255.255.255.255") {
		return nil, fmt.Errorf("operator: gateway must be a unicast IPv4 address")
	}
	raw := bytes.Clone(s.IdentRaw)
	if len(raw) < 6 || len(raw) > 65507 || uint64(binary.BigEndian.Uint32(raw[:4])) != uint64(len(raw)-6) {
		return nil, fmt.Errorf("operator: invalid identification datagram length")
	}
	f, err := hsfz.ReadFrame(bytes.NewReader(raw), 65501)
	if err != nil {
		return nil, fmt.Errorf("operator: identification frame: %w", err)
	}
	if f.Control != hsfz.ControlVehicleIdent {
		return nil, fmt.Errorf("operator: identification uses control 0x%04x", f.Control)
	}
	if _, err := hsfz.ParseIdentification(f.Body); err != nil {
		return nil, fmt.Errorf("operator: identification fields: %w", err)
	}
	return &vehicle{address: ip, raw: raw}, nil
}

// Addr returns independently assigned diagnostic, control and discovery endpoints.
func (p *Presenter) Addr() (diag, ctrl *net.TCPAddr, disc *net.UDPAddr) {
	return p.diag.Addr().(*net.TCPAddr), p.ctrl.Addr().(*net.TCPAddr), p.disc.LocalAddr()
}

// ExclusiveState returns a detached snapshot shared by both TCP listeners.
func (p *Presenter) ExclusiveState() exclusive.Snapshot {
	if p.gate == nil {
		return exclusive.Snapshot{}
	}
	return p.gate.Snapshot()
}

// Update copies the discovery datagram before publishing the next vehicle snapshot.
// An empty gateway and datagram suspend new connections and discovery responses.
func (p *Presenter) Update(state proto.VehicleState) error {
	v, err := parseState(state)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ctx.Err() != nil {
		return fmt.Errorf("operator: update: %w", net.ErrClosed)
	}
	p.state = v
	return nil
}

func (p *Presenter) snapshot() *vehicle {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

// Close cancels pending opens and copies, releases all listeners, and joins workers.
func (p *Presenter) Close() error {
	p.once.Do(func() {
		p.cancel()
		if p.diag != nil {
			p.diag.Close()
		}
		if p.ctrl != nil {
			p.ctrl.Close()
		}
		if p.disc != nil {
			p.disc.Close()
		}
		p.mu.Lock()
		for conn := range p.connections {
			conn.Close()
		}
		p.mu.Unlock()
	})
	p.wg.Wait()
	return nil
}

func (p *Presenter) accept(ln *net.TCPListener, port uint16, channel string) {
	defer p.wg.Done()
	for {
		conn, err := ln.AcceptTCP()
		if err != nil {
			if p.ctx.Err() == nil {
				p.logger.ErrorContext(p.ctx, "gateway listener failed", "error", err)
				p.cancel()
			}
			return
		}
		p.mu.Lock()
		state := p.state
		if p.ctx.Err() != nil || state == nil || p.maintenance {
			p.mu.Unlock()
			conn.Close()
			continue
		}
		p.connections[conn] = struct{}{}
		p.wg.Add(1)
		p.mu.Unlock()
		go p.forward(conn, state, port, channel)
	}
}

func (p *Presenter) forward(conn *net.TCPConn, state *vehicle, port uint16, channel string) {
	defer p.wg.Done()
	var lease *exclusive.Lease
	var capture *hsfzcapture.Connection
	var activity tunnel.Activity
	var err error
	target := net.JoinHostPort(state.address.String(), strconv.Itoa(int(port)))
	if p.observer != nil {
		activity = p.observer.Begin(target)
	}
	defer func() {
		conn.Close()
		if lease != nil {
			lease.Release()
		}
		p.mu.Lock()
		delete(p.connections, conn)
		p.mu.Unlock()
		if capture != nil {
			capture.End(err)
		}
		if activity != nil {
			activity.End(err)
		}
	}()
	if p.gate != nil {
		lease, err = p.gate.Admit(p.ctx, conn.LocalAddr().(*net.TCPAddr).AddrPort(), conn.RemoteAddr().(*net.TCPAddr).AddrPort())
		if err != nil {
			if p.ctx.Err() == nil {
				p.logger.WarnContext(p.ctx, "gateway connection rejected", "peer", conn.RemoteAddr().String(), "error", err)
			}
			return
		}
	}
	if p.capture != nil {
		capture = p.capture.Begin(hsfzcapture.Metadata{Generation: p.generation(), Network: "tcp", Channel: channel, Local: conn.LocalAddr().String(), Peer: conn.RemoteAddr().String(), Target: target})
	}
	stream, err := p.opener.OpenTunnel(p.ctx, proto.OpenTunnel{Kind: "tcp", Target: target, Origin: conn.RemoteAddr().String()})
	if err != nil {
		if p.ctx.Err() == nil {
			p.logger.WarnContext(p.ctx, "open gateway tunnel failed", "target", target, "error", err)
		}
		return
	}
	var stats *tunnel.Stats
	if activity != nil {
		stats = &tunnel.Stats{Counter: activity}
	}
	if capture != nil {
		if stats == nil {
			stats = &tunnel.Stats{}
		}
		stats.Payload = captureTap{capture}
	}
	err = tunnel.Pipe(p.ctx, stream, conn, stats)
	if err != nil && p.ctx.Err() == nil {
		p.logger.DebugContext(p.ctx, "gateway tunnel closed", "target", target, "error", err)
	}
}

func (p *Presenter) discover() {
	defer p.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, peer, err := p.disc.Read(p.ctx, buf)
		if err != nil {
			if p.ctx.Err() == nil {
				p.logger.ErrorContext(p.ctx, "discovery listener failed", "error", err)
				p.cancel()
			}
			return
		}
		state := p.snapshot()
		meta := hsfzcapture.Metadata{}
		if p.capture != nil {
			meta = hsfzcapture.Metadata{Generation: p.generation(), Network: "udp", Channel: "discovery", Local: p.disc.LocalAddr().String(), Peer: peer.String()}
			if state != nil {
				meta.Target = net.JoinHostPort(state.address.String(), "6811")
			}
			p.capture.Datagram(meta, hsfzcapture.ToOperator, buf[:n])
		}
		if !bytes.Equal(buf[:n], hsfz.IdentRequest) {
			continue
		}
		if state == nil {
			continue
		}
		written, err := p.disc.Write(p.ctx, state.raw, peer)
		if p.capture != nil && err == nil && written == len(state.raw) {
			// This is the locally cached identification response, not a new vehicle read.
			p.capture.Datagram(meta, hsfzcapture.FromOperator, state.raw[:written])
		}
		if err != nil && !errors.Is(err, net.ErrClosed) && p.ctx.Err() == nil {
			p.logger.WarnContext(p.ctx, "send gateway discovery failed", "peer", peer.String(), "error", err)
		}
	}
}

// WithMaintenance reserves an idle presenter and joins its callback during Close.
// The callback runs outside the presenter lock and must honor cancellation.
func (p *Presenter) WithMaintenance(ctx context.Context, fn func(context.Context, netip.Addr) error) error {
	p.mu.Lock()
	if err := ctx.Err(); err != nil {
		p.mu.Unlock()
		return err
	}
	if p.ctx.Err() != nil || p.state == nil {
		p.mu.Unlock()
		return net.ErrClosed
	}
	if p.maintenance || len(p.connections) != 0 || p.gate != nil && p.gate.Snapshot().Active != 0 {
		p.mu.Unlock()
		return exclusive.ErrBusy
	}
	p.maintenance = true
	address := p.state.address
	p.wg.Add(1)
	p.mu.Unlock()
	defer p.wg.Done()
	defer func() { p.mu.Lock(); p.maintenance = false; p.mu.Unlock() }()
	opCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(p.ctx, cancel)
	defer stop()
	defer cancel()
	if p.ctx.Err() != nil {
		cancel()
	}
	err := fn(opCtx, address)
	if err != nil {
		return err
	}
	return opCtx.Err()
}

func (p *Presenter) generation() uint64 {
	if p.captureGeneration == nil {
		return 0
	}
	return p.captureGeneration()
}

type captureTap struct{ connection *hsfzcapture.Connection }

func (t captureTap) In(b []byte)  { t.connection.Record(hsfzcapture.FromVehicle, b) }
func (t captureTap) Out(b []byte) { t.connection.Record(hsfzcapture.ToVehicle, b) }
