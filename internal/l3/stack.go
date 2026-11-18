// Package l3 presents allowlisted IPv4 TCP destinations through tunnel streams.
package l3

import (
	"context"
	"fmt"
	"io"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/waiter"

	"remote-enet/internal/packetio"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
	"remote-enet/internal/tunnel"
)

// Opener must unblock when its context is canceled.
type Opener interface {
	OpenTunnel(context.Context, proto.OpenTunnel) (transport.Stream, error)
}

// Options bounds both half-open handshakes and established forwarding attempts.
type Options struct {
	Targets  []netip.Addr
	Opener   Opener
	Observer tunnel.Observer
	// MaxConnections defaults to 64; explicit values must be in 1..65535.
	MaxConnections int
	// Admit runs after the handshake and must return when its context is canceled.
	Admit func(context.Context, netip.AddrPort, netip.AddrPort) (func(), error)
}

// Snapshot counts reserved attempts, successfully opened streams, and refusals.
type Snapshot struct{ Active, Accepted, Rejected uint64 }

// Stack owns its TCP endpoints and workers, but never the supplied device.
type Stack struct {
	ctx                        context.Context
	cancel                     context.CancelFunc
	net                        *stack.Stack
	link                       *channel.Endpoint
	device                     packetio.Device
	opts                       Options
	mu                         sync.Mutex
	pending                    map[stack.TransportEndpointID]bool
	workers                    sync.WaitGroup
	ioWorkers                  sync.WaitGroup
	active, accepted, rejected atomic.Uint64
	done                       chan struct{}
	failure                    error
}

// Start does not configure host interfaces or routes. Targets are exact IPv4 addresses.
func Start(ctx context.Context, device packetio.Device, opts Options) (*Stack, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if device == nil || opts.Opener == nil {
		return nil, fmt.Errorf("l3: device and opener are required")
	}
	if device.MTU() < 576 || device.MTU() > 65535 {
		return nil, fmt.Errorf("l3: invalid MTU %d", device.MTU())
	}
	if opts.MaxConnections == 0 {
		opts.MaxConnections = 64
	}
	if opts.MaxConnections < 1 || opts.MaxConnections > 65535 {
		return nil, fmt.Errorf("l3: MaxConnections outside 1..65535")
	}
	targets := make(map[netip.Addr]bool)
	if len(opts.Targets) == 0 {
		return nil, fmt.Errorf("l3: targets are required")
	}
	for _, addr := range opts.Targets {
		if !addr.Is4() || addr.IsUnspecified() || addr.IsLoopback() || addr.IsMulticast() || addr == netip.MustParseAddr("255.255.255.255") || targets[addr] {
			return nil, fmt.Errorf("l3: invalid or duplicate target %s", addr)
		}
		targets[addr] = true
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &Stack{ctx: ctx, cancel: cancel, device: device, opts: opts, pending: make(map[stack.TransportEndpointID]bool), done: make(chan struct{})}
	s.net = stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	s.link = channel.New(opts.MaxConnections, uint32(device.MTU()), "")
	if err := s.net.CreateNIC(1, s.link); err != nil {
		cancel()
		s.net.Destroy()
		return nil, fmt.Errorf("l3: create NIC: %s", err)
	}
	for addr := range targets {
		pa := tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddrFrom4(addr.As4()).WithPrefix()}
		if err := s.net.AddProtocolAddress(1, pa, stack.AddressProperties{}); err != nil {
			cancel()
			s.net.Destroy()
			return nil, fmt.Errorf("l3: add address: %s", err)
		}
	}
	s.net.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	forwarder := tcp.NewForwarder(s.net, 0, opts.MaxConnections, s.forward)
	s.net.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.ctx.Err() != nil {
			return true
		}
		if s.pending[id] {
			return true
		}
		// Reserve before Forwarder can spawn a worker, including during shutdown.
		if s.active.Load() >= uint64(opts.MaxConnections) {
			s.rejected.Add(1)
			return true
		}
		s.pending[id] = true
		s.active.Add(1)
		s.workers.Add(1)
		if !forwarder.HandlePacket(id, pkt) {
			delete(s.pending, id)
			s.active.Add(^uint64(0))
			s.workers.Done()
			return false
		}
		return true
	})
	s.ioWorkers.Add(2)
	go s.read(targets)
	go s.write()
	go s.shutdown()
	return s, nil
}

func (s *Stack) forward(r *tcp.ForwarderRequest) {
	id := r.ID()
	defer func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		s.active.Add(^uint64(0))
		s.workers.Done()
	}()
	if s.ctx.Err() != nil {
		r.Complete(true)
		return
	}
	var q waiter.Queue
	ep, err := r.CreateEndpoint(&q)
	r.Complete(err != nil)
	if err != nil {
		s.rejected.Add(1)
		return
	}
	conn := gonet.NewTCPConn(&q, ep)
	defer conn.Close()
	dst := netip.AddrPortFrom(netip.AddrFrom4(id.LocalAddress.As4()), id.LocalPort)
	src := netip.AddrPortFrom(netip.AddrFrom4(id.RemoteAddress.As4()), id.RemotePort)
	var activity tunnel.Activity
	var forwardErr error
	if s.opts.Observer != nil {
		activity = s.opts.Observer.Begin(dst.String())
	}
	defer func() {
		if activity != nil {
			activity.End(forwardErr)
		}
	}()
	if s.opts.Admit != nil {
		release, err := s.opts.Admit(s.ctx, dst, src)
		if release != nil {
			defer release()
		}
		if err != nil {
			s.rejected.Add(1)
			forwardErr = err
			return
		}
	}
	stream, openErr := s.opts.Opener.OpenTunnel(s.ctx, proto.OpenTunnel{Kind: "tcp", Target: dst.String(), Origin: src.String()})
	if openErr != nil {
		forwardErr = openErr
		if stream != nil {
			_ = stream.Close()
		}
		s.rejected.Add(1)
		return
	}
	s.accepted.Add(1)
	var stats *tunnel.Stats
	if activity != nil {
		stats = &tunnel.Stats{Counter: activity}
	}
	forwardErr = tunnel.Pipe(s.ctx, stream, conn, stats)
}

func (s *Stack) read(targets map[netip.Addr]bool) {
	defer s.ioWorkers.Done()
	defer s.cancel()
	buf := make([]byte, s.device.MTU())
	for {
		n, err := s.device.ReadPacket(s.ctx, buf)
		if err != nil {
			if err == io.ErrShortBuffer {
				continue
			}
			s.fail(fmt.Errorf("l3: read device: %w", err))
			return
		}
		// Filter before reassembly and TCP dispatch; unknown protocols never reach Opener.
		if n < header.IPv4MinimumSize || buf[0]>>4 != 4 || buf[9] != byte(tcp.ProtocolNumber) {
			s.rejected.Add(1)
			continue
		}
		dst := netip.AddrFrom4([4]byte(buf[16:20]))
		if !targets[dst] {
			s.rejected.Add(1)
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), buf[:n]...))})
		s.link.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()
	}
}

func (s *Stack) write() {
	defer s.ioWorkers.Done()
	defer s.cancel()
	for {
		pkt := s.link.ReadContext(s.ctx)
		if pkt == nil {
			return
		}
		view := pkt.ToView()
		data := view.AsSlice()
		n, err := s.device.WritePacket(s.ctx, data)
		view.Release()
		pkt.DecRef()
		if err != nil || n != len(data) {
			if err == nil {
				err = io.ErrShortWrite
			}
			s.fail(fmt.Errorf("l3: write device: %w", err))
			return
		}
	}
}

func (s *Stack) shutdown() {
	<-s.ctx.Done()
	s.ioWorkers.Wait()
	// Exclude new reservations before waiting, including a callback queued by gVisor.
	s.mu.Lock()
	s.mu.Unlock()
	joined := make(chan struct{})
	go func() { s.workers.Wait(); close(joined) }()
	// Forwarder.CreateEndpoint exposes no context until the handshake completes.
	// Abort registered endpoints repeatedly to cover concurrent handshake registration.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		for _, ep := range s.net.RegisteredEndpoints() {
			ep.Abort()
		}
		select {
		case <-joined:
			s.link.Close()
			s.net.Destroy()
			close(s.done)
			return
		case <-ticker.C:
		}
	}
}

// Done closes when shutdown starts; Close joins all workers afterwards.
func (s *Stack) Done() <-chan struct{} { return s.ctx.Done() }

// Err reports the first unexpected device failure, excluding normal cancellation.
func (s *Stack) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failure
}

func (s *Stack) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() == nil && s.failure == nil {
		s.failure = err
	}
	s.cancel()
}

// Close cancels and joins all workers; the caller must close its device separately.
func (s *Stack) Close() error { s.cancel(); <-s.done; return nil }

// Snapshot is safe during forwarding and after Close.
func (s *Stack) Snapshot() Snapshot {
	return Snapshot{Active: s.active.Load(), Accepted: s.accepted.Load(), Rejected: s.rejected.Load()}
}
