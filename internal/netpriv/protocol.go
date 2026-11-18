package netpriv

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"remote-enet/internal/packetio"
)

const maxPacket = 1500
const queueLimit = 16
const handshakeTimeout = 10 * time.Second

type wire struct {
	net.Conn
	mu sync.Mutex
}

func (w *wire) send(kind byte, p []byte) error {
	if len(p) > maxPacket {
		return fmt.Errorf("netpriv: frame exceeds budget")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	b := make([]byte, 3+len(p))
	b[0] = kind
	binary.BigEndian.PutUint16(b[1:], uint16(len(p)))
	copy(b[3:], p)
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func (w *wire) receive() (byte, []byte, error) {
	var h [3]byte
	if _, e := io.ReadFull(w, h[:]); e != nil {
		return 0, nil, e
	}
	n := int(binary.BigEndian.Uint16(h[1:]))
	if n > maxPacket {
		return 0, nil, fmt.Errorf("netpriv: frame exceeds budget")
	}
	b := make([]byte, n)
	_, e := io.ReadFull(w, b)
	return h[0], b, e
}

type metadata struct {
	Name  string
	Local netip.Prefix
}
type ownedDevice interface {
	packetio.Device
	Name() string
	Local() netip.Prefix
}
type opener func(context.Context, []netip.Addr) (ownedDevice, error)

// allowedPacket rejects options, fragments and malformed TCP headers before privileged I/O.
func allowedPacket(p []byte, src, dst netip.Addr) bool {
	if len(p) < 40 || len(p) > maxPacket || p[0] != 0x45 || p[8] == 0 || p[9] != 6 || int(binary.BigEndian.Uint16(p[2:4])) != len(p) || binary.BigEndian.Uint16(p[6:8])&0xbfff != 0 {
		return false
	}
	if netip.AddrFrom4([4]byte(p[12:16])) != src || netip.AddrFrom4([4]byte(p[16:20])) != dst {
		return false
	}
	h := int(p[32]>>4) * 4
	if h < 20 || 20+h > len(p) || p[32]&0x0e != 0 || checksum(p[:20], 0) != 0 {
		return false
	}
	pseudo := uint32(6 + len(p) - 20)
	for i := 12; i < 20; i += 2 {
		pseudo += uint32(binary.BigEndian.Uint16(p[i:]))
	}
	return checksum(p[20:], pseudo) == 0
}

func checksum(p []byte, sum uint32) uint16 {
	for len(p) >= 2 {
		sum += uint32(binary.BigEndian.Uint16(p))
		p = p[2:]
	}
	if len(p) > 0 {
		sum += uint32(p[0]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 65535) + (sum >> 16)
	}
	return ^uint16(sum)
}

// serve owns the native device until both forwarding loops and cleanup finish.
func serve(ctx context.Context, c net.Conn, open opener) (err error) {
	w := &wire{Conn: c}
	defer c.Close()
	life, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(life, func() { _ = c.Close() })
	defer stop()
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	k, p, e := w.receive()
	if e != nil || k != 'H' || len(p) != 4 {
		return fmt.Errorf("netpriv: invalid handshake")
	}
	target := netip.AddrFrom4([4]byte(p))
	if !validTarget(target) {
		return fmt.Errorf("netpriv: invalid target")
	}
	// A live reader is installed before setup so EOF cancels an in-flight OpenAuto.
	incoming := make(chan []byte, 1)
	var readers sync.WaitGroup
	readers.Go(func() {
		for {
			k, p, e := w.receive()
			if e != nil {
				cancel()
				return
			}
			if k == 'C' && len(p) == 0 {
				cancel()
				return
			}
			if k != 'P' {
				cancel()
				return
			}
			select {
			case incoming <- p:
			case <-life.Done():
				return
			default:
				// Never stop reading EOF behind a blocked native writer.
				cancel()
				return
			}
		}
	})
	// The handshake timer also bounds setup and a client that stalls before setup finishes.
	setupTimer := time.AfterFunc(handshakeTimeout, cancel)
	a, e := open(life, []netip.Addr{target})
	setupTimer.Stop()
	if e != nil {
		cancel()
		readers.Wait()
		return fmt.Errorf("netpriv: network setup failed")
	}
	// Cancellation closes the IPC, so graceful close uses a separate cleanup acknowledgment below.
	stop()
	cleanup := func() error { cancel(); _ = c.Close(); readers.Wait(); return a.Close() }
	if life.Err() != nil {
		return cleanup()
	}
	if a.MTU() != maxPacket || !a.Local().IsValid() || a.Local().Bits() != 32 || !validTarget(a.Local().Addr()) || a.Local().Addr() == target || len(a.Name()) > 128 {
		return cleanup()
	}
	b, _ := json.Marshal(metadata{Name: a.Name(), Local: a.Local()})
	if e = w.send('M', b); e != nil {
		return cleanup()
	}
	_ = c.SetDeadline(time.Time{})
	var workers sync.WaitGroup
	failures := make(chan error, 2)
	workers.Go(func() {
		buf := make([]byte, maxPacket)
		for {
			n, e := a.ReadPacket(life, buf)
			if e != nil {
				failures <- e
				return
			}
			if allowedPacket(buf[:n], a.Local().Addr(), target) {
				if e = w.send('P', buf[:n]); e != nil {
					failures <- e
					return
				}
			}
		}
	})
	workers.Go(func() {
		for {
			select {
			case <-life.Done():
				return
			case p := <-incoming:
				if !allowedPacket(p, target, a.Local().Addr()) {
					failures <- fmt.Errorf("netpriv: rejected packet")
					return
				}
				n, e := a.WritePacket(life, p)
				if e == nil && n != len(p) {
					e = io.ErrShortWrite
				}
				if e == nil {
					e = w.send('W', nil)
				}
				if e != nil {
					failures <- e
					return
				}
			}
		}
	})
	var forwardingErr error
	select {
	case <-life.Done():
	case e := <-failures:
		if life.Err() == nil {
			forwardingErr = e
		}
		cancel()
	case <-ctx.Done():
		cancel()
	}
	// No writer may race the final acknowledgment. Bound a stalled peer first.
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	cancel()
	closeErr := a.Close()
	workers.Wait()
	// Closing read half wakes the reader without discarding the write half.
	if uc, ok := c.(*net.UnixConn); ok {
		_ = uc.CloseRead()
	} else {
		_ = c.SetReadDeadline(time.Now())
	}
	readers.Wait()
	_ = c.SetWriteDeadline(time.Now().Add(time.Second))
	payload := []byte{}
	if closeErr != nil {
		payload = []byte{1}
	} else if forwardingErr != nil {
		payload = []byte{2}
	}
	_ = w.send('C', payload)
	return errors.Join(forwardingErr, closeErr)
}

type writeRequest struct {
	ctx    context.Context
	p      []byte
	result chan error
}
type streamDevice struct {
	w         *wire
	local     netip.Addr
	target    netip.Addr
	reads     chan []byte
	writes    chan writeRequest
	done      chan struct{}
	stopped   chan struct{}
	closing   chan struct{}
	gate      chan struct{}
	acks      chan struct{}
	awaiting  atomic.Bool
	once      sync.Once
	closeOnce sync.Once
	wg        sync.WaitGroup
	mu        sync.Mutex
	err       error
}

func newStream(ctx context.Context, w *wire, m metadata, target netip.Addr) *Device {
	d := &streamDevice{w: w, local: m.Local.Addr(), target: target, reads: make(chan []byte, queueLimit), writes: make(chan writeRequest), done: make(chan struct{}), stopped: make(chan struct{}), closing: make(chan struct{}), gate: make(chan struct{}, 1), acks: make(chan struct{}, 1)}
	d.wg.Go(d.readLoop)
	d.wg.Go(d.writeLoop)
	go func() {
		select {
		case <-ctx.Done():
			_ = d.Close()
		case <-d.done:
		}
	}()
	return &Device{Device: d, name: m.Name, local: m.Local}
}
func (d *streamDevice) finish(e error) {
	d.once.Do(func() { d.mu.Lock(); d.err = e; d.mu.Unlock(); close(d.done); _ = d.w.Close() })
}
func (d *streamDevice) readLoop() {
	for {
		k, p, e := d.w.receive()
		if e != nil {
			d.finish(fmt.Errorf("netpriv: helper disconnected before cleanup confirmation"))
			return
		}
		if k == 'W' {
			if len(p) != 0 || !d.awaiting.Swap(false) {
				d.finish(fmt.Errorf("netpriv: unexpected write confirmation"))
				return
			}
			d.acks <- struct{}{}
			continue
		}
		if k == 'C' {
			if len(p) == 1 && p[0] == 2 {
				d.finish(fmt.Errorf("netpriv: helper network I/O failed"))
			} else if len(p) != 0 {
				d.finish(fmt.Errorf("netpriv: helper cleanup failed"))
			} else {
				d.finish(nil)
			}
			return
		}
		if k != 'P' || !allowedPacket(p, d.local, d.target) {
			d.finish(fmt.Errorf("netpriv: invalid helper packet"))
			return
		}
		select {
		case d.reads <- p:
		case <-d.closing:
		case <-d.done:
			return
		default:
			// Packet delivery is lossy under backpressure, but control frames
			// must remain readable while native writes await confirmation.
		}
	}
}
func (d *streamDevice) writeLoop() {
	for {
		select {
		case <-d.done:
			return
		case r := <-d.writes:
			if e := r.ctx.Err(); e != nil {
				r.result <- e
				continue
			}
			d.awaiting.Store(true)
			e := d.w.send('P', r.p)
			if e == nil {
				// Even a canceled caller leaves its confirmation owned by this worker.
				select {
				case <-d.acks:
				case <-d.done:
					e = io.ErrClosedPipe
				}
			}
			r.result <- e
			if e != nil {
				d.finish(fmt.Errorf("netpriv: helper write failed"))
				return
			}
		}
	}
}
func (d *streamDevice) MTU() int { return maxPacket }
func (d *streamDevice) ReadPacket(ctx context.Context, b []byte) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.done:
		return 0, io.EOF
	case p := <-d.reads:
		if len(b) < len(p) {
			return 0, io.ErrShortBuffer
		}
		return copy(b, p), nil
	}
}
func (d *streamDevice) WritePacket(ctx context.Context, p []byte) (int, error) {
	if e := ctx.Err(); e != nil {
		return 0, e
	}
	if !allowedPacket(p, d.target, d.local) {
		return 0, fmt.Errorf("netpriv: rejected packet")
	}
	select {
	case d.gate <- struct{}{}:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.done:
		return 0, io.ErrClosedPipe
	}
	defer func() { <-d.gate }()
	r := writeRequest{ctx: ctx, p: append([]byte(nil), p...), result: make(chan error, 1)}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.done:
		return 0, io.ErrClosedPipe
	case d.writes <- r:
	}
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-d.done:
		return 0, io.ErrClosedPipe
	case e := <-r.result:
		if e != nil {
			return 0, e
		}
		return len(p), nil
	}
}
func (d *streamDevice) Close() error {
	d.closeOnce.Do(func() {
		close(d.closing)
		// A blocked packet write must not prevent shutdown from making progress.
		_ = d.w.SetWriteDeadline(time.Now().Add(time.Second))
		sent := make(chan error, 1)
		go func() { sent <- d.w.send('C', nil) }()
		select {
		case <-sent:
		case <-d.done:
			<-sent
		}
		timer := time.NewTimer(12 * time.Second)
		select {
		case <-d.done:
		case <-timer.C:
			d.finish(fmt.Errorf("netpriv: cleanup confirmation timed out"))
		}
		timer.Stop()
		d.finish(nil)
		d.wg.Wait()
		close(d.stopped)
	})
	<-d.stopped
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func handshake(ctx context.Context, c net.Conn, target netip.Addr) (*Device, error) {
	w := &wire{Conn: c}
	_ = c.SetDeadline(time.Now().Add(handshakeTimeout))
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	a := target.As4()
	if e := w.send('H', a[:]); e != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("netpriv: helper handshake failed")
	}
	k, p, e := w.receive()
	if e != nil || k != 'M' {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("netpriv: helper setup failed")
	}
	var m metadata
	if json.Unmarshal(p, &m) != nil || !m.Local.IsValid() || m.Local.Bits() != 32 || !validTarget(m.Local.Addr()) || m.Local.Addr() == target || len(m.Name) == 0 || len(m.Name) > 128 {
		return nil, fmt.Errorf("netpriv: invalid helper metadata")
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	_ = c.SetDeadline(time.Time{})
	return newStream(ctx, w, m, target), nil
}

var _ packetio.Device = (*streamDevice)(nil)
