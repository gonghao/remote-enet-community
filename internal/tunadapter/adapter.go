// Package tunadapter owns a newly created TUN and its explicit IPv4 host routes.
package tunadapter

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/tun"
	"remote-enet/internal/packetio"
)

// Config never grants ownership of an existing interface or route.
// Name is a Linux interface name or Darwin utun/utunN. On Windows it is
// a label prefix; a random suffix prevents Wintun from renaming foreign adapters.
type Config struct {
	Name    string
	Local   netip.Prefix
	Targets []netip.Addr
	MTU     int
}
type result struct {
	packet []byte
	err    error
}
type writeRequest struct {
	ctx    context.Context
	packet []byte
	reply  chan result
}
type operation struct{ add, remove []string }

type routeEntry struct {
	Prefix                netip.Prefix
	Gateway, Flags, Netif string
}

var nativeReadiness func(context.Context, tun.Device, netip.Prefix) error

type backend struct {
	interfaces func() ([]net.Interface, error)
	addresses  func(net.Interface) ([]net.Addr, error)
	routes     func(context.Context) ([]routeEntry, error)
	create     func(string, int) (tun.Device, error)
	run        func(context.Context, []string) error
	ready      func(context.Context, tun.Device, netip.Prefix) error
	os         string
}

// Adapter has one native reader and writer; per-call cancellation leaves them alive.
type Adapter struct {
	native   tun.Device
	name     string
	mtu      int
	local    netip.Prefix
	done     chan struct{}
	closed   chan struct{}
	reads    chan result
	writes   chan writeRequest
	gate     chan struct{}
	once     sync.Once
	workers  sync.WaitGroup
	undo     []operation
	run      func(context.Context, []string) error
	closeErr error
}

var _ packetio.Device = (*Adapter)(nil)

// Open validates the entire configuration before acquiring system resources.
func Open(ctx context.Context, c Config) (*Adapter, error) { return open(ctx, c, systemBackend()) }

func validate(c Config, platform string) (Config, error) {
	if c.MTU == 0 {
		c.MTU = 1500
	}
	if c.MTU < 576 || c.MTU > 65535 {
		return c, fmt.Errorf("tunadapter: MTU outside 576..65535")
	}
	if !c.Local.IsValid() || !validIP(c.Local.Addr()) || c.Local.Bits() != 32 {
		return c, fmt.Errorf("tunadapter: local must be unicast IPv4 /32")
	}
	if len(c.Targets) == 0 || len(c.Targets) > 256 {
		return c, fmt.Errorf("tunadapter: targets outside 1..256")
	}
	seen := make(map[netip.Addr]bool)
	c.Targets = append([]netip.Addr(nil), c.Targets...)
	for _, ip := range c.Targets {
		if !validIP(ip) || ip == c.Local.Addr() || seen[ip] {
			return c, fmt.Errorf("tunadapter: invalid or duplicate target %v", ip)
		}
		seen[ip] = true
	}
	if c.Name == "" {
		if platform == "darwin" {
			c.Name = "utun"
		} else {
			c.Name = fmt.Sprintf("enet%x", randomID())
		}
	}
	pattern := `^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`
	if platform == "darwin" {
		pattern = `^utun([0-9]{1,5})?$`
	}
	if !regexp.MustCompile(pattern).MatchString(c.Name) {
		return c, fmt.Errorf("tunadapter: invalid %s device name %q", platform, c.Name)
	}
	return c, nil
}
func randomID() []byte {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return b
}
func validIP(ip netip.Addr) bool {
	return ip.Is4() && (ip.IsGlobalUnicast() || ip.IsLinkLocalUnicast()) && ip.As4()[0] != 0 && ip != netip.MustParseAddr("255.255.255.255")
}

func open(ctx context.Context, c Config, b backend) (*Adapter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var err error
	c, err = validate(c, b.os)
	if err != nil {
		return nil, err
	}
	if err := checkAvailability(ctx, c, b); err != nil {
		return nil, err
	}
	native, err := b.create(c.Name, c.MTU)
	if err != nil {
		return nil, fmt.Errorf("create TUN: %w", err)
	}
	a := &Adapter{native: native, local: c.Local, mtu: c.MTU, done: make(chan struct{}), closed: make(chan struct{}), reads: make(chan result, 16), writes: make(chan writeRequest), gate: make(chan struct{}, 1), run: b.run}
	// Drain events even during configuration and native Close.
	a.workers.Go(func() {
		for range native.Events() {
		}
	})
	a.name, err = native.Name()
	if err == nil {
		pattern := `^[a-zA-Z][a-zA-Z0-9_-]{0,14}$`
		if b.os == "windows" {
			pattern = `^[a-zA-Z][a-zA-Z0-9_-]{0,127}$`
		}
		if !regexp.MustCompile(pattern).MatchString(a.name) {
			err = fmt.Errorf("invalid native interface name %q", a.name)
		}
	}
	if err == nil && (native.BatchSize() < 1 || native.BatchSize() > 128) {
		err = fmt.Errorf("unsupported native batch size %d", native.BatchSize())
	}
	if err == nil {
		for _, op := range plan(b.os, a.name, c) {
			if err = b.run(ctx, op.add); err != nil {
				break
			}
			if len(op.remove) > 0 {
				a.undo = append(a.undo, op)
			}
		}
	}
	if err == nil && b.ready != nil {
		err = b.ready(ctx, native, c.Local)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("configure TUN: %w", err), a.Close())
	}
	a.workers.Go(a.readLoop)
	a.workers.Go(a.writeLoop)
	go func() {
		select {
		case <-ctx.Done():
			_ = a.Close()
		case <-a.done:
		}
	}()
	if err = ctx.Err(); err != nil {
		return nil, errors.Join(err, a.Close())
	}
	return a, nil
}

// Name returns the actual allocated interface name, including any private suffix.
func (a *Adapter) Name() string { return a.name }

// MTU is fixed for the lifetime of the adapter.
func (a *Adapter) MTU() int { return a.mtu }

// ReadPacket drops a packet on a short buffer rather than exposing a partial IP packet.
func (a *Adapter) ReadPacket(ctx context.Context, buffer []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-a.done:
		return 0, io.EOF
	case r, ok := <-a.reads:
		if !ok {
			return 0, io.EOF
		}
		if r.err != nil {
			return 0, r.err
		}
		if len(buffer) < len(r.packet) {
			return 0, io.ErrShortBuffer
		}
		return copy(buffer, r.packet), nil
	}
}

// WritePacket copies caller memory. Cancellation cannot retract an already started native write.
func (a *Adapter) WritePacket(ctx context.Context, packet []byte) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(packet) == 0 || len(packet) > a.mtu {
		return 0, fmt.Errorf("tunadapter: packet length outside 1..%d", a.mtu)
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-a.done:
		return 0, io.ErrClosedPipe
	case a.gate <- struct{}{}:
	}
	defer func() { <-a.gate }()
	req := writeRequest{ctx: ctx, packet: append([]byte(nil), packet...), reply: make(chan result, 1)}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-a.done:
		return 0, io.ErrClosedPipe
	case a.writes <- req:
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-a.done:
		return 0, io.ErrClosedPipe
	case r := <-req.reply:
		if r.err != nil {
			return 0, r.err
		}
		return len(packet), nil
	}
}
func (a *Adapter) readLoop() {
	defer close(a.reads)
	batch := a.native.BatchSize()
	if batch < 1 || batch > 128 {
		select {
		case a.reads <- result{err: fmt.Errorf("tunadapter: unsupported batch size %d", batch)}:
		case <-a.done:
		}
		return
	}
	buffers := make([][]byte, batch)
	sizes := make([]int, batch)
	for i := range buffers {
		buffers[i] = make([]byte, 65535+4)
	}
	for {
		n, err := a.native.Read(buffers, sizes, 4)
		if err != nil {
			select {
			case a.reads <- result{err: fmt.Errorf("read native TUN: %w", err)}:
			case <-a.done:
			}
			return
		}
		if n < 0 || n > batch {
			return
		}
		for i := 0; i < n; i++ {
			if sizes[i] < 1 || sizes[i] > a.mtu {
				continue
			}
			r := result{packet: append([]byte(nil), buffers[i][4:4+sizes[i]]...)}
			select {
			case a.reads <- r:
			case <-a.done:
				return
			}
		}
		select {
		case <-a.done:
			return
		default:
		}
	}
}
func (a *Adapter) writeLoop() {
	for {
		select {
		case <-a.done:
			return
		case req := <-a.writes:
			if err := req.ctx.Err(); err != nil {
				req.reply <- result{err: err}
				continue
			}
			buffer := make([]byte, 4+len(req.packet))
			copy(buffer[4:], req.packet)
			n, err := a.native.Write([][]byte{buffer}, 4)
			if err == nil && n != 1 {
				err = io.ErrShortWrite
			}
			if err != nil {
				err = fmt.Errorf("write native TUN: %w", err)
			}
			req.reply <- result{err: err}
		}
	}
}

// Close removes owned routes and addresses before closing the native device.
// Its stored error also reports cleanup failures triggered by the opening context.
func (a *Adapter) Close() error {
	a.once.Do(func() {
		close(a.done)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		for i := len(a.undo) - 1; i >= 0; i-- {
			err := a.run(ctx, a.undo[i].remove)
			if err != nil {
				a.closeErr = errors.Join(a.closeErr, fmt.Errorf("cleanup %v: %w", a.undo[i].remove, err))
			}
		}
		a.closeErr = errors.Join(a.closeErr, a.native.Close())
		a.workers.Wait()
		close(a.closed)
	})
	<-a.closed
	return a.closeErr
}

// ErrConflict identifies a local-address conflict detected before creating a TUN.
var ErrConflict = errors.New("tunadapter: local address conflict")

// Local is the explicit host address owned by this adapter.
func (a *Adapter) Local() netip.Prefix { return a.local }

// OpenAuto tries a bounded set of host addresses, skipping only preflight conflicts.
func OpenAuto(ctx context.Context, targets []netip.Addr) (*Adapter, error) {
	return openAuto(ctx, targets, systemBackend())
}
func openAuto(ctx context.Context, targets []netip.Addr, b backend) (*Adapter, error) {
	var err error
	for _, ip := range autoAddresses {
		local := netip.MustParseAddr(ip)
		collision := false
		for _, target := range targets {
			if target == local {
				collision = true
			}
		}
		if collision {
			err = ErrConflict
			continue
		}
		var a *Adapter
		a, err = open(ctx, Config{Local: netip.PrefixFrom(local, 32), Targets: targets}, b)
		if !errors.Is(err, ErrConflict) {
			return a, err
		}
	}
	return nil, err
}

// This recognizes an observed route shape, not the provenance of the route.
func darwinLinkLocalFallback(platform string, route routeEntry, target netip.Addr, interfaces []net.Interface) bool {
	if platform != "darwin" || route.Prefix != netip.MustParsePrefix("169.254.0.0/16") || !target.Is4() || !target.IsLinkLocalUnicast() {
		return false
	}
	if route.Flags != "UCS" && route.Flags != "UCSI" {
		return false
	}
	if !regexp.MustCompile(`^en[0-9]+$`).MatchString(route.Netif) || !strings.HasPrefix(route.Gateway, "link#") {
		return false
	}
	indexText := strings.TrimPrefix(route.Gateway, "link#")
	if indexText == "" {
		return false
	}
	for _, digit := range indexText {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	index, err := strconv.Atoi(indexText)
	if err != nil || index <= 0 {
		return false
	}
	for _, iface := range interfaces {
		if iface.Name == route.Netif && iface.Index == index {
			return iface.Flags&(net.FlagUp|net.FlagBroadcast) == net.FlagUp|net.FlagBroadcast && iface.Flags&(net.FlagLoopback|net.FlagPointToPoint) == 0
		}
	}
	return false
}
