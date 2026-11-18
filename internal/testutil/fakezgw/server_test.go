package fakezgw

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
)

func profile() Profile {
	return Profile{Ident: hsfz.Identification{DiagAddr: 0x10, VIN: "WBAXXXXXXX0000001", MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}}}
}
func start(t *testing.T, p Profile) *Server {
	t.Helper()
	s, err := Start(t.Context(), netip.MustParseAddr("127.0.0.1"), p, WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func dial(t *testing.T, a *net.TCPAddr) *net.TCPConn {
	t.Helper()
	c, err := net.DialTCP("tcp", nil, a)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}
func read(t *testing.T, c net.Conn) hsfz.Frame {
	t.Helper()
	f, err := hsfz.ReadFrame(c, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func write(t *testing.T, c net.Conn, f hsfz.Frame) {
	t.Helper()
	if err := hsfz.WriteFrame(c, f); err != nil {
		t.Fatal(err)
	}
}
func waitStats(t *testing.T, s *Server, ok func(Stats) bool) {
	t.Helper()
	end := time.NewTimer(2 * time.Second)
	defer end.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if ok(s.Stats()) {
			return
		}
		select {
		case <-end.C:
			t.Fatalf("stats: %+v", s.Stats())
		case <-tick.C:
		}
	}
}
func TestDiscovery(t *testing.T) {
	s := start(t, profile())
	_, _, a := s.Addr()
	c, err := net.DialUDP("udp", nil, a)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := make([]byte, 256)
	for _, bad := range [][]byte{{}, hsfz.IdentRequest[:5], {0, 0, 0, 0, 0, 0x10}, append(bytes.Clone(hsfz.IdentRequest), 0)} {
		c.Write(bad)
		c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		if _, err := c.Read(b); err == nil {
			t.Fatal("replied to invalid discovery")
		}
	}
	c.SetDeadline(time.Now().Add(time.Second))
	c.Write(hsfz.IdentRequest)
	n, err := c.Read(b)
	if err != nil {
		t.Fatal(err)
	}
	if n != 56 {
		t.Fatal(n)
	}
	f, err := hsfz.ReadFrame(bytes.NewReader(b[:n]), 50)
	if err != nil {
		t.Fatal(err)
	}
	id, err := hsfz.ParseIdentification(f.Body)
	if err != nil || id.VIN != profile().Ident.VIN {
		t.Fatal(id, err)
	}
}
func TestDiagAndControl(t *testing.T) {
	p := profile()
	kl := byte(0x04)
	p.KL15 = &kl
	p.Responders = map[string][]byte{"22F101": {0x62, 0xf1, 1}}
	p.MultiResponders = map[string][]hsfz.DiagBody{"1902": {{Src: 0x10, Payload: []byte{0x59, 2}}, {Src: 0x63, Payload: []byte{0x59, 2, 1}}}}
	s := start(t, p)
	a, ctrl, _ := s.Addr()
	c := dial(t, a)
	req := hsfz.DiagBody{Src: 0xf4, Dst: 0x63, Payload: []byte{0x22, 0xf1, 1}}
	raw, _ := (hsfz.Frame{Control: hsfz.ControlDiag, Body: req.Marshal()}).MarshalBinary()
	for _, b := range raw {
		c.Write([]byte{b})
	}
	ack := read(t, c)
	if ack.Control != hsfz.ControlAck || !bytes.Equal(ack.Body, req.Marshal()) {
		t.Fatal(ack)
	}
	r := read(t, c)
	if r.Control != hsfz.ControlDiag || !bytes.Equal(r.Body, []byte{0x63, 0xf4, 0x62, 0xf1, 1}) {
		t.Fatal(r)
	}
	req.Dst = 0xdf
	req.Payload = []byte{0x19, 2}
	f := hsfz.Frame{Control: hsfz.ControlDiag, Body: req.Marshal()}
	b, _ := f.MarshalBinary()
	c.Write(append(bytes.Clone(b), b...))
	for i := 0; i < 2; i++ {
		if read(t, c).Control != hsfz.ControlAck {
			t.Fatal("ACK order")
		}
		for _, src := range []byte{0x10, 0x63} {
			r := read(t, c)
			if r.Body[0] != src || r.Body[1] != 0xf4 {
				t.Fatal(r)
			}
		}
	}
	cc := dial(t, ctrl)
	write(t, cc, hsfz.Frame{Control: hsfz.ControlTerminal15})
	r = read(t, cc)
	if r.Control != hsfz.ControlTerminal15 || !bytes.Equal(r.Body, []byte{kl}) {
		t.Fatal(r)
	}
	waitStats(t, s, func(st Stats) bool { return st.DiagReceived == 3 && st.ACKSent == 3 })
}
func TestUnknown(t *testing.T) {
	for _, negative := range []bool{false, true} {
		t.Run(map[bool]string{false: "timeout", true: "NRC"}[negative], func(t *testing.T) {
			p := profile()
			if negative {
				p.Unknown = NegativeResponse(0x11)
			}
			s := start(t, p)
			a, _, _ := s.Addr()
			c := dial(t, a)
			write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x99}})
			if read(t, c).Control != hsfz.ControlAck {
				t.Fatal("no ACK")
			}
			if negative {
				f := read(t, c)
				if !bytes.Equal(f.Body, []byte{0x63, 0xf4, 0x7f, 0x99, 0x11}) {
					t.Fatal(f)
				}
			} else {
				c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
				if _, err := hsfz.ReadFrame(c, 100); err == nil {
					t.Fatal("unknown succeeded")
				}
			}
		})
	}
}
func TestAlive(t *testing.T) {
	for _, long := range []bool{false, true} {
		t.Run(map[bool]string{false: "short", true: "long"}[long], func(t *testing.T) {
			p := profile()
			p.AliveInterval = 10 * time.Millisecond
			p.AliveTimeout = 150 * time.Millisecond
			p.AliveLongBody = long
			p.ShortAliveBody = &[2]byte{0x12, 0x34}
			p.TesterAddr = 0xf5
			s := start(t, p)
			a, _, _ := s.Addr()
			c := dial(t, a)
			for i := 0; i < 3; i++ {
				f := read(t, c)
				if f.Control != hsfz.ControlAliveCheck || len(f.Body) != map[bool]int{false: 2, true: 32}[long] {
					t.Fatal(f)
				}
				if long && !bytes.Equal(f.Body, []byte("DIAGADR10BMWVINWBAXXXXXXX0000001")) {
					t.Fatal(f)
				}
				write(t, c, hsfz.AliveResponse(0xf5))
			}
			waitStats(t, s, func(st Stats) bool { return st.AliveReceived == 3 })
			f := read(t, c)
			if f.Control != hsfz.ControlAliveCheck {
				t.Fatal(f)
			}
			write(t, c, hsfz.AliveResponse(0xf4))
			if _, err := hsfz.ReadFrame(c, 100); err == nil {
				t.Fatal("wrong tester kept alive")
			}
			waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 0 })
		})
	}
}
func TestAliveIsolationAndNoTimeout(t *testing.T) {
	p := profile()
	p.AliveInterval = 10 * time.Millisecond
	p.AliveLongBody = true
	p.AliveTimeout = 100 * time.Millisecond
	s := start(t, p)
	a, _, _ := s.Addr()
	silent := dial(t, a)
	active := dial(t, a)
	read(t, silent)
	for i := 0; i < 15; i++ {
		read(t, active)
		write(t, active, hsfz.AliveResponse(0xf4))
	}
	if _, err := hsfz.ReadFrame(silent, 100); err == nil {
		t.Fatal("silent connection survived")
	}
	waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 1 })
	p.AliveTimeout = 0
	s2 := start(t, p)
	a, _, _ = s2.Addr()
	c := dial(t, a)
	for i := 0; i < 3; i++ {
		if read(t, c).Control != hsfz.ControlAliveCheck {
			t.Fatal("no repeated alive")
		}
	}
}
func TestFaultsAndReconnect(t *testing.T) {
	for _, fault := range []Fault{FaultRST, FaultHalfClose, FaultIncorrectControl, FaultOutOfMemory} {
		t.Run(string(fault), func(t *testing.T) {
			p := profile()
			p.Faults = []Fault{fault}
			s := start(t, p)
			a, _, _ := s.Addr()
			for i := 0; i < 2; i++ {
				c, err := net.DialTCP("tcp", nil, a)
				if err != nil {
					if fault != FaultRST || !errors.Is(err, syscall.ECONNRESET) {
						t.Fatal(err)
					}
					// The peer's injected reset may reach the dialer before Dial
					// returns, rather than being deferred until the first Read.
					waitStats(t, s, func(st Stats) bool { return st.Connections == uint64(i+1) && st.ActiveConns == 0 })
					continue
				}
				if err := c.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
					c.Close()
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				switch fault {
				case FaultRST:
					_, err := c.Read(make([]byte, 1))
					if !errors.Is(err, syscall.ECONNRESET) {
						t.Fatalf("expected RST: %v", err)
					}
				case FaultHalfClose:
					if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
						t.Fatal(err)
					}
					write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22}})
					c.CloseWrite()
				default:
					f := read(t, c)
					want := hsfz.ControlErrIncorrectControlWord
					if fault == FaultOutOfMemory {
						want = hsfz.ControlErrOutOfMemory
					}
					if f.Control != want || len(f.Body) != 0 {
						t.Fatal(f)
					}
					write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22}})
					if read(t, c).Control != hsfz.ControlAck {
						t.Fatal("parser blocked after error")
					}
				}
				c.Close()
				waitStats(t, s, func(st Stats) bool { return st.Connections == uint64(i+1) && st.ActiveConns == 0 })
			}
		})
	}
}
func TestCapacityCancelAndHalfClose(t *testing.T) {
	p := profile()
	p.MaxConns = 1
	s := start(t, p)
	a, _, _ := s.Addr()
	c := dial(t, a)
	waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 1 })
	rejected := dial(t, a)
	if _, err := rejected.Read(make([]byte, 1)); err == nil {
		t.Fatal("second accepted")
	}
	c.Close()
	waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 0 })
	c = dial(t, a)
	write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x10, 0x22}})
	c.CloseWrite()
	if read(t, c).Control != hsfz.ControlAck {
		t.Fatal("lost response after FIN")
	}
	if _, err := hsfz.ReadFrame(c, 100); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s2, err := Start(ctx, netip.MustParseAddr("127.0.0.1"), profile(), WithPorts(0, 0, 0))
	if err != nil {
		t.Fatal(err)
	}
	a, ctrl, _ := s2.Addr()
	c = dial(t, a)
	cc := dial(t, ctrl)
	c.Write([]byte{0})
	waitStats(t, s2, func(st Stats) bool { return st.ActiveConns == 2 })
	cancel()
	s2.Close()
	if _, err := cc.Read(make([]byte, 1)); err == nil {
		t.Fatal("control not closed")
	}
	if s2.Stats().ActiveConns != 0 {
		t.Fatal(s2.Stats())
	}
}
func TestParallelAndLimits(t *testing.T) {
	p := profile()
	s := start(t, p)
	a, _, _ := s.Addr()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Go(func() {
			c := dial(t, a)
			defer c.Close()
			for n := 0; n < 5; n++ {
				write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22}})
				if read(t, c).Control != hsfz.ControlAck {
					t.Error("bad ACK")
				}
			}
		})
	}
	wg.Wait()
	waitStats(t, s, func(st Stats) bool { return st.ACKSent == 60 && st.ActiveConns == 0 })
	c := dial(t, a)
	c.Write([]byte{0xff, 0xff, 0xff, 0xff, 0, 1})
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("oversize not closed")
	}
	c = dial(t, a)
	write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4}})
	write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22}})
	if read(t, c).Control != hsfz.ControlAck {
		t.Fatal("malformed request blocked parser")
	}
}
func TestValidationAndBindFailure(t *testing.T) {
	for _, modify := range []func(*Profile){func(p *Profile) { p.Ident.VIN = "bad" }, func(p *Profile) { p.AliveInterval = time.Second }, func(p *Profile) { p.Unknown = NegativeResponse(0) }, func(p *Profile) { p.Responders = map[string][]byte{"xx": {}} }, func(p *Profile) { p.Faults = []Fault{"unknown"} }} {
		p := profile()
		modify(&p)
		if s, err := Start(t.Context(), netip.MustParseAddr("127.0.0.1"), p, WithPorts(0, 0, 0)); err == nil {
			s.Close()
			t.Fatal("invalid profile accepted")
		}
	}
	s := start(t, profile())
	a, _, _ := s.Addr()
	if other, err := Start(t.Context(), netip.MustParseAddr("127.0.0.1"), profile(), WithPorts(0, a.Port, 0)); err == nil {
		other.Close()
		t.Fatal("bind collision accepted")
	}
}
