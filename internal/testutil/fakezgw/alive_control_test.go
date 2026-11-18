package fakezgw

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
)

func TestExplicitAliveResponseControl(t *testing.T) {
	for _, configured := range []hsfz.Control{0, hsfz.ControlStatusInquiry, hsfz.ControlAliveCheck} {
		for _, response := range []hsfz.Control{hsfz.ControlStatusInquiry, hsfz.ControlAliveCheck} {
			t.Run(fmt.Sprintf("configured-%04x-response-%04x", configured, response), func(t *testing.T) {
				p := profile()
				p.AliveInterval = 10 * time.Millisecond
				p.AliveTimeout = 300 * time.Millisecond
				p.AliveLongBody = true
				p.AliveResponseControl = configured
				s := start(t, p)
				addr, _, _ := s.Addr()
				c := dial(t, addr)
				if f := read(t, c); f.Control != hsfz.ControlAliveCheck {
					t.Fatalf("initial alive: %+v", f)
				}
				write(t, c, hsfz.Frame{Control: response, Body: []byte{0, 0xf4}})
				expected := configured
				if expected == 0 {
					expected = hsfz.ControlStatusInquiry
				}
				if response != expected {
					if f, err := hsfz.ReadFrame(c, 100); err == nil {
						t.Fatalf("unexpected control kept connection alive: %+v", f)
					}
					waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 0 })
					if got := s.Stats().AliveReceived; got != 0 {
						t.Fatalf("accepted wrong control: %d", got)
					}
					return
				}
				waitStats(t, s, func(st Stats) bool { return st.AliveReceived == 1 })
				if f := read(t, c); f.Control != hsfz.ControlAliveCheck {
					t.Fatalf("next alive: %+v", f)
				}
				write(t, c, hsfz.Frame{Control: response, Body: []byte{0, 0xf4}})
				waitStats(t, s, func(st Stats) bool { return st.AliveReceived == 2 })
			})
		}
	}
}

func TestInvalidAliveResponseControl(t *testing.T) {
	p := profile()
	p.AliveResponseControl = hsfz.ControlAck
	if s, err := Start(t.Context(), netip.MustParseAddr("127.0.0.1"), p, WithPorts(0, 0, 0)); err == nil {
		s.Close()
		t.Fatal("invalid alive response control accepted")
	}
}
