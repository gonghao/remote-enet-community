// Package udpdiscovery receives broadcasts on one interface and pins reply sources.
package udpdiscovery

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"time"
)

type endpoint struct {
	bind  netip.Addr
	index int
	bcast netip.Addr
}

// Conn owns a wildcard socket but exposes only the selected interface/address.
// Concurrent reads and writes are serialized independently; operation cancellation
// interrupts its own I/O without closing an otherwise usable connection.
type Conn struct {
	socket    *net.UDPConn
	owner     context.Context
	local     endpoint
	port      int
	read      chan struct{}
	write     chan struct{}
	done      chan struct{}
	ownerDone chan struct{}
	stop      func() bool
	once      sync.Once
	err       error
}

// Listen owns the socket until Close or ctx cancellation. Bind selects a source
// address/interface; receiving uses a wildcard address so broadcasts can arrive.
func Listen(ctx context.Context, bind netip.Addr, port int) (*Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if port < 0 || port > 65535 {
		return nil, fmt.Errorf("udpdiscovery: port %d outside 0..65535", port)
	}
	local, err := selectEndpoint(bind)
	if err != nil {
		return nil, err
	}
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: port})
	if err != nil {
		return nil, fmt.Errorf("udpdiscovery: listen: %w", err)
	}
	if err := enablePacketInfo(socket); err != nil {
		socket.Close()
		return nil, fmt.Errorf("udpdiscovery: enable packet info: %w", err)
	}
	c := &Conn{socket: socket, owner: ctx, local: local, port: socket.LocalAddr().(*net.UDPAddr).Port,
		read: make(chan struct{}, 1), write: make(chan struct{}, 1), done: make(chan struct{}), ownerDone: make(chan struct{})}
	// The callback touches only the initialized socket, never the stop field.
	c.stop = context.AfterFunc(ctx, func() { _ = socket.Close(); close(c.ownerDone) })
	if err := ctx.Err(); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

// LocalAddr reports the reply address, not the wildcard receive binding.
func (c *Conn) LocalAddr() *net.UDPAddr {
	return net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.local.bind, uint16(c.port)))
}

// Read discards missing/truncated metadata and datagrams for other interfaces or
// unicast addresses. It never retains b after returning.
func (c *Conn) Read(ctx context.Context, b []byte) (int, *net.UDPAddr, error) {
	if len(b) == 0 {
		return 0, nil, fmt.Errorf("udpdiscovery: receive buffer must not be empty")
	}
	if err := c.acquire(ctx, c.read); err != nil {
		return 0, nil, err
	}
	defer func() { <-c.read }()
	finish, err := ioDeadline(ctx, c.socket.SetReadDeadline)
	if err != nil {
		return 0, nil, c.ioError(ctx, err)
	}
	defer finish()
	oob := make([]byte, 256)
	for {
		n, oobn, flags, peer, err := c.socket.ReadMsgUDP(b, oob)
		if flags&truncatedFlags != 0 || truncatedRead(err) {
			if ctx.Err() != nil || c.owner.Err() != nil {
				return 0, nil, c.ioError(ctx, err)
			}
			continue
		}
		if err != nil {
			return 0, nil, c.ioError(ctx, fmt.Errorf("udpdiscovery: receive: %w", err))
		}
		if peer == nil || peer.IP.To4() == nil || peer.Port == 0 {
			continue
		}
		peerIP, _ := netip.AddrFromSlice(peer.IP.To4())
		if !c.unicastPeer(peerIP) {
			continue
		}
		info, err := parsePacketInfo(oob[:oobn])
		if err != nil || !c.local.accepts(info) {
			continue
		}
		return n, peer, nil
	}
}

// Write preserves the selected source IP, receive port, and outgoing interface.
// Empty replies are rejected because BSD sendmsg inserts a dummy byte when
// ancillary data has no payload; discovery must never silently change bytes.
func (c *Conn) Write(ctx context.Context, b []byte, peer *net.UDPAddr) (int, error) {
	if len(b) == 0 {
		return 0, fmt.Errorf("udpdiscovery: reply payload must not be empty")
	}
	if peer == nil || peer.Port < 1 || peer.Port > 65535 || peer.IP.To4() == nil {
		return 0, fmt.Errorf("udpdiscovery: reply peer must be IPv4 with a nonzero port")
	}
	ip, _ := netip.AddrFromSlice(peer.IP.To4())
	if !c.unicastPeer(ip) {
		return 0, fmt.Errorf("udpdiscovery: reply peer must be a unicast address")
	}
	if err := c.acquire(ctx, c.write); err != nil {
		return 0, err
	}
	defer func() { <-c.write }()
	finish, err := ioDeadline(ctx, c.socket.SetWriteDeadline)
	if err != nil {
		return 0, c.ioError(ctx, err)
	}
	defer finish()
	oob := marshalPacketInfo(c.local.bind, c.local.index)
	n, controlN, err := c.socket.WriteMsgUDP(b, oob, net.UDPAddrFromAddrPort(netip.AddrPortFrom(ip, uint16(peer.Port))))
	if err != nil {
		return n, c.ioError(ctx, fmt.Errorf("udpdiscovery: reply: %w", err))
	}
	// Go's Windows WSASendMsg path can leave the byte count at zero on an
	// immediate successful completion. UDP sends are atomic: account for the
	// accepted datagram without resending it or ignoring any actual I/O error.
	if runtime.GOOS == "windows" && n == 0 && len(b) > 0 && controlN == len(oob) {
		n = len(b)
	}
	if n != len(b) || controlN != len(oob) {
		return n, fmt.Errorf("udpdiscovery: reply: %w", io.ErrShortWrite)
	}
	return n, nil
}

// Close releases pending I/O and is safe to call repeatedly or concurrently.
func (c *Conn) Close() error {
	c.once.Do(func() {
		close(c.done)
		if !c.stop() {
			<-c.ownerDone
		}
		c.err = c.socket.Close()
		if errors.Is(c.err, net.ErrClosed) {
			c.err = nil
		}
	})
	return c.err
}

func (c *Conn) acquire(ctx context.Context, gate chan struct{}) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.owner.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.owner.Done():
		return c.owner.Err()
	case <-c.done:
		return net.ErrClosed
	case gate <- struct{}{}:
		return nil
	}
}

func (c *Conn) ioError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var timeout net.Error
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) && errors.As(err, &timeout) && timeout.Timeout() {
		return context.DeadlineExceeded
	}
	if c.owner.Err() != nil {
		return c.owner.Err()
	}
	return err
}

func ioDeadline(ctx context.Context, set func(time.Time) error) (func(), error) {
	deadline, _ := ctx.Deadline()
	if err := set(deadline); err != nil {
		return nil, err
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = set(time.Now()); close(done) })
	return func() {
		if !stop() {
			<-done
		}
		_ = set(time.Time{})
	}, nil
}

func (e endpoint) accepts(info packetInfo) bool {
	return info.index == e.index && (info.dst == e.bind || info.dst == e.bcast || info.dst == netip.AddrFrom4([4]byte{255, 255, 255, 255}))
}

func (c *Conn) unicastPeer(ip netip.Addr) bool {
	return !ip.IsUnspecified() && !ip.IsMulticast() && ip != netip.AddrFrom4([4]byte{255, 255, 255, 255}) &&
		ip != c.local.bcast
}

func selectEndpoint(bind netip.Addr) (endpoint, error) {
	if !bind.Is4() || !bind.IsGlobalUnicast() && !bind.IsLoopback() && !bind.IsLinkLocalUnicast() || bind == netip.AddrFrom4([4]byte{255, 255, 255, 255}) {
		return endpoint{}, fmt.Errorf("udpdiscovery: bind must be a specific unicast IPv4 address")
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return endpoint{}, fmt.Errorf("udpdiscovery: enumerate interfaces: %w", err)
	}
	var exact, loopback []endpoint
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Index <= 0 {
			continue
		}
		addresses, err := iface.Addrs()
		if err != nil {
			return endpoint{}, fmt.Errorf("udpdiscovery: addresses on %s: %w", iface.Name, err)
		}
		for _, address := range addresses {
			prefix, err := netip.ParsePrefix(address.String())
			if err != nil || !prefix.Addr().Is4() {
				continue
			}
			local := endpoint{bind: bind, index: iface.Index}
			if prefix.Bits() < 31 {
				local.bcast = broadcastAddress(prefix)
				if bind == local.bcast || bind == prefix.Masked().Addr() {
					continue
				}
			}
			if prefix.Addr() == bind {
				exact = append(exact, local)
			} else if bind.IsLoopback() && iface.Flags&net.FlagLoopback != 0 && prefix.Contains(bind) {
				loopback = append(loopback, local)
			}
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = loopback
	}
	if len(matches) != 1 {
		return endpoint{}, fmt.Errorf("udpdiscovery: bind %s matches %d active IPv4 interfaces", bind, len(matches))
	}
	return matches[0], nil
}

func broadcastAddress(prefix netip.Prefix) netip.Addr {
	b := prefix.Masked().Addr().As4()
	for bit := prefix.Bits(); bit < 32; bit++ {
		b[bit/8] |= 1 << (7 - uint(bit%8))
	}
	return netip.AddrFrom4(b)
}
