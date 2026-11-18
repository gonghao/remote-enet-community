// Package fakezgw provides configurable test behavior, not a vehicle simulator.
package fakezgw

import (
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"time"

	"remote-enet/internal/hsfz"
)

// UnknownPolicy defaults to silence; negative responses require an explicit NRC.
type UnknownPolicy struct {
	NegativeResponse bool
	NRC              byte
}

var Timeout = UnknownPolicy{}

func NegativeResponse(nrc byte) UnknownPolicy { return UnknownPolicy{true, nrc} }

// Faults are applied on each diagnostic connection before reading requests.
type Fault string

const (
	FaultRST              Fault = "rst"
	FaultHalfClose        Fault = "half-close"
	FaultIncorrectControl Fault = "incorrect-control"
	FaultOutOfMemory      Fault = "out-of-memory"
)

// All timing and capacity values are test parameters, not measured ZGW defaults.
type Profile struct {
	Ident         hsfz.Identification
	TesterAddr    byte
	AliveInterval time.Duration
	AliveTimeout  time.Duration
	AliveLongBody bool
	// AliveResponseControl defaults to 0x0013. 0x0012 is an explicit tool experiment.
	AliveResponseControl hsfz.Control
	// ShortAliveBody is required when sending short alive checks: its content is unknown.
	ShortAliveBody *[2]byte
	MaxConns       int
	Responders     map[string][]byte
	// MultiResponders supplies explicit physical source addresses for functional requests.
	// Entries take precedence over Responders; destination is always the request tester.
	MultiResponders map[string][]hsfz.DiagBody
	Unknown         UnknownPolicy
	Faults          []Fault
	// KL15 must be configured to enable simulated terminal status responses.
	KL15 *byte
}

type options struct {
	diag, ctrl, disc int
	maxBody          int
	logger           *slog.Logger
}
type Option func(*options)

// WithPorts accepts zero for OS-assigned ports, independently for all listeners.
func WithPorts(diag, ctrl, disc int) Option {
	return func(o *options) { o.diag = diag; o.ctrl = ctrl; o.disc = disc }
}

// WithMaxBody bounds memory used by each incoming frame; default is 1 MiB.
func WithMaxBody(n int) Option         { return func(o *options) { o.maxBody = n } }
func WithLogger(l *slog.Logger) Option { return func(o *options) { o.logger = l } }

// Stats is a consistent snapshot; Connections counts accepted connections cumulatively.
type Stats struct{ Connections, ActiveConns, RejectedConns, DiagReceived, ACKSent, AliveSent, AliveReceived uint64 }
type Server struct {
	p          Profile
	o          options
	ctx        context.Context
	cancel     context.CancelFunc
	diag, ctrl *net.TCPListener
	disc       *net.UDPConn
	mu         sync.Mutex
	stats      Stats
	conns      map[*net.TCPConn]struct{}
	wg         sync.WaitGroup
	once       sync.Once
}

// Start binds all three endpoints atomically and stops them when ctx is canceled.
// Profile collections and pointed values are copied; callers may reuse them after Start.
func Start(ctx context.Context, bind netip.Addr, p Profile, opts ...Option) (*Server, error) {
	o := options{diag: 6801, ctrl: 6811, disc: 6811, maxBody: 1 << 20}
	for _, opt := range opts {
		opt(&o)
	}
	if o.diag < 0 || o.diag > 65535 || o.ctrl < 0 || o.ctrl > 65535 || o.disc < 0 || o.disc > 65535 || !bind.IsValid() || o.maxBody < 0 || p.AliveInterval < 0 || p.AliveTimeout < 0 || p.MaxConns < 0 {
		return nil, fmt.Errorf("invalid fake ZGW address, limit or timing")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start fake ZGW: %w", err)
	}
	if len(p.Ident.VIN) != 17 {
		return nil, fmt.Errorf("identification VIN must be 17 ASCII bytes")
	}
	if _, err := hsfz.ParseIdentification(p.Ident.MarshalBody()); err != nil {
		return nil, fmt.Errorf("validate identification: %w", err)
	}
	if p.AliveInterval > 0 && !p.AliveLongBody && p.ShortAliveBody == nil {
		return nil, fmt.Errorf("short alive body must be explicitly configured")
	}
	if p.AliveResponseControl == 0 {
		p.AliveResponseControl = hsfz.ControlStatusInquiry
	}
	if p.AliveResponseControl != hsfz.ControlStatusInquiry && p.AliveResponseControl != hsfz.ControlAliveCheck {
		return nil, fmt.Errorf("alive response control must be 0x0013 or experimental 0x0012")
	}
	if p.Unknown.NegativeResponse && p.Unknown.NRC == 0 {
		return nil, fmt.Errorf("negative response requires a nonzero NRC")
	}
	for _, f := range p.Faults {
		switch f {
		case FaultRST, FaultHalfClose, FaultIncorrectControl, FaultOutOfMemory:
		default:
			return nil, fmt.Errorf("unknown fault %q", f)
		}
	}
	p.Ident.Raw = bytes.Clone(p.Ident.Raw)
	p.Faults = append([]Fault(nil), p.Faults...)
	responders := make(map[string][]byte, len(p.Responders))
	for k, v := range p.Responders {
		b, err := hex.DecodeString(k)
		if err != nil {
			return nil, fmt.Errorf("responder key %q: %w", k, err)
		}
		responders[hex.EncodeToString(b)] = bytes.Clone(v)
	}
	p.Responders = responders
	multi := make(map[string][]hsfz.DiagBody, len(p.MultiResponders))
	for k, v := range p.MultiResponders {
		b, err := hex.DecodeString(k)
		if err != nil {
			return nil, fmt.Errorf("multi responder key %q: %w", k, err)
		}
		key := hex.EncodeToString(b)
		multi[key] = make([]hsfz.DiagBody, 0, len(v))
		for _, d := range v {
			d.Payload = bytes.Clone(d.Payload)
			multi[key] = append(multi[key], d)
		}
	}
	p.MultiResponders = multi
	if p.KL15 != nil {
		v := *p.KL15
		p.KL15 = &v
	}
	if p.ShortAliveBody != nil {
		v := *p.ShortAliveBody
		p.ShortAliveBody = &v
	}
	if p.TesterAddr == 0 {
		p.TesterAddr = 0xf4
	}
	serverCtx, cancel := context.WithCancel(ctx)
	s := &Server{p: p, o: o, ctx: serverCtx, cancel: cancel, conns: make(map[*net.TCPConn]struct{})}
	var err error
	s.diag, err = net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(bind, uint16(o.diag))))
	if err != nil {
		cancel()
		return nil, fmt.Errorf("listen diagnostic: %w", err)
	}
	s.ctrl, err = net.ListenTCP("tcp", net.TCPAddrFromAddrPort(netip.AddrPortFrom(bind, uint16(o.ctrl))))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("listen control: %w", err)
	}
	s.disc, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(bind, uint16(o.disc))))
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("listen discovery: %w", err)
	}
	s.wg.Add(3)
	go s.accept(s.diag, false)
	go s.accept(s.ctrl, true)
	go s.discover()
	context.AfterFunc(serverCtx, func() { s.Close() })
	return s, nil
}
func (s *Server) Addr() (diag, ctrl *net.TCPAddr, disc *net.UDPAddr) {
	return s.diag.Addr().(*net.TCPAddr), s.ctrl.Addr().(*net.TCPAddr), s.disc.LocalAddr().(*net.UDPAddr)
}
func (s *Server) Stats() Stats         { s.mu.Lock(); defer s.mu.Unlock(); return s.stats }
func (s *Server) count(f func(*Stats)) { s.mu.Lock(); defer s.mu.Unlock(); f(&s.stats) }

// Close waits for listeners and connection workers, and is safe to call repeatedly.
func (s *Server) Close() error {
	s.once.Do(func() {
		s.cancel()
		if s.diag != nil {
			s.diag.Close()
		}
		if s.ctrl != nil {
			s.ctrl.Close()
		}
		if s.disc != nil {
			s.disc.Close()
		}
		s.mu.Lock()
		for c := range s.conns {
			c.Close()
		}
		s.mu.Unlock()
	})
	s.wg.Wait()
	return nil
}
func (s *Server) log(msg string, args ...any) {
	if s.o.logger != nil {
		s.o.logger.Info(msg, args...)
	}
}
func (s *Server) discover() {
	defer s.wg.Done()
	b := make([]byte, 65535)
	for {
		n, addr, err := s.disc.ReadFromUDP(b)
		if err != nil {
			return
		}
		s.log("discovery receive", "peer", addr.String(), "hex", hex.EncodeToString(b[:n]))
		if !bytes.Equal(b[:n], hsfz.IdentRequest) {
			continue
		}
		raw, _ := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: s.p.Ident.MarshalBody()}).MarshalBinary()
		if _, err = s.disc.WriteToUDP(raw, addr); err != nil {
			if s.ctx.Err() != nil {
				return
			}
			continue
		}
		s.log("discovery send", "peer", addr.String(), "hex", hex.EncodeToString(raw))
	}
}
func (s *Server) accept(ln *net.TCPListener, control bool) {
	defer s.wg.Done()
	for {
		c, err := ln.AcceptTCP()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.stats.Connections++
		if s.ctx.Err() != nil || (s.p.MaxConns > 0 && len(s.conns) >= s.p.MaxConns) {
			s.stats.RejectedConns++
			s.mu.Unlock()
			c.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.stats.ActiveConns++
		s.wg.Add(1)
		s.mu.Unlock()
		go s.serve(c, control)
	}
}
func (s *Server) send(c *net.TCPConn, f hsfz.Frame) error {
	raw, _ := f.MarshalBinary()
	s.log("frame send", "peer", c.RemoteAddr().String(), "hex", hex.EncodeToString(raw))
	return hsfz.WriteFrame(c, f)
}
func (s *Server) serve(c *net.TCPConn, control bool) {
	defer s.wg.Done()
	defer func() {
		c.Close()
		s.mu.Lock()
		delete(s.conns, c)
		s.stats.ActiveConns--
		s.mu.Unlock()
		s.log("connection closed", "peer", c.RemoteAddr().String())
	}()
	s.log("connection accepted", "peer", c.RemoteAddr().String(), "control", control)
	if !control {
		for _, fault := range s.p.Faults {
			switch fault {
			case FaultRST:
				c.SetLinger(0)
				return
			case FaultHalfClose:
				c.CloseWrite()
			case FaultIncorrectControl:
				if s.send(c, hsfz.Frame{Control: hsfz.ControlErrIncorrectControlWord}) != nil {
					return
				}
			case FaultOutOfMemory:
				if s.send(c, hsfz.Frame{Control: hsfz.ControlErrOutOfMemory}) != nil {
					return
				}
			}
		}
	}
	ctx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	frames := make(chan hsfz.Frame)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer close(frames)
		for {
			f, err := hsfz.ReadFrame(c, s.o.maxBody)
			if err != nil {
				return
			}
			select {
			case frames <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	defer func() { cancel(); c.Close(); <-done }()
	var tick <-chan time.Time
	var ticker *time.Ticker
	if !control && s.p.AliveInterval > 0 {
		ticker = time.NewTicker(s.p.AliveInterval)
		tick = ticker.C
		defer ticker.Stop()
	}
	var timer *time.Timer
	pending := false
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
			if pending {
				continue
			}
			body := []byte(nil)
			if s.p.AliveLongBody {
				body = []byte(fmt.Sprintf("DIAGADR%02XBMWVIN%s", s.p.Ident.DiagAddr, s.p.Ident.VIN))
			} else {
				body = append(body, s.p.ShortAliveBody[:]...)
			}
			if s.send(c, hsfz.Frame{Control: hsfz.ControlAliveCheck, Body: body}) != nil {
				return
			}
			s.count(func(st *Stats) { st.AliveSent++ })
			pending = true
			if s.p.AliveTimeout > 0 {
				// Closing from the timer also interrupts a blocked diagnostic write.
				timer = time.AfterFunc(s.p.AliveTimeout, func() { c.Close() })
			} else {
				pending = false
			}
		case f, ok := <-frames:
			if !ok {
				return
			}
			raw, _ := f.MarshalBinary()
			s.log("frame receive", "peer", c.RemoteAddr().String(), "hex", hex.EncodeToString(raw))
			if control {
				if f.Control == hsfz.ControlTerminal15 && len(f.Body) == 0 && s.p.KL15 != nil {
					if s.send(c, hsfz.Frame{Control: hsfz.ControlTerminal15, Body: []byte{*s.p.KL15}}) != nil {
						return
					}
				}
				continue
			}
			switch f.Control {
			case hsfz.ControlStatusInquiry, hsfz.ControlAliveCheck:
				if f.Control == s.p.AliveResponseControl && bytes.Equal(f.Body, []byte{0, s.p.TesterAddr}) {
					s.count(func(st *Stats) { st.AliveReceived++ })
					if pending {
						pending = false
						timer.Stop()
					}
				}
			case hsfz.ControlDiag:
				d, err := hsfz.ParseDiagBody(f.Body)
				if err != nil || len(d.Payload) == 0 {
					continue
				}
				s.count(func(st *Stats) { st.DiagReceived++ })
				if s.send(c, hsfz.AckFor(d)) != nil {
					return
				}
				s.count(func(st *Stats) { st.ACKSent++ })
				key := hex.EncodeToString(d.Payload)
				responses, ok := s.p.MultiResponders[key]
				if !ok {
					if payload, found := s.p.Responders[key]; found {
						responses = []hsfz.DiagBody{{Src: d.Dst, Payload: payload}}
					} else if s.p.Unknown.NegativeResponse {
						responses = []hsfz.DiagBody{{Src: d.Dst, Payload: []byte{0x7f, d.Payload[0], s.p.Unknown.NRC}}}
					}
				}
				for _, r := range responses {
					r.Dst = d.Src
					if s.send(c, hsfz.Frame{Control: hsfz.ControlDiag, Body: r.Marshal()}) != nil {
						return
					}
				}
			}
		}
	}
}
