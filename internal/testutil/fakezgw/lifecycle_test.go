package fakezgw

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"remote-enet/internal/hsfz"
)

func TestBindFailureReleasesEarlierListeners(t *testing.T) {
	bind := netip.MustParseAddr("127.0.0.1")
	for _, failUDP := range []bool{false, true} {
		t.Run(map[bool]string{false: "control", true: "discovery"}[failUDP], func(t *testing.T) {
			diag, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				t.Fatal(err)
			}
			diagPort := diag.Addr().(*net.TCPAddr).Port
			ctrl, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				diag.Close()
				t.Fatal(err)
			}
			ctrlPort := ctrl.Addr().(*net.TCPAddr).Port
			defer ctrl.Close()
			disc, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			if err != nil {
				diag.Close()
				t.Fatal(err)
			}
			defer disc.Close()
			discPort := disc.LocalAddr().(*net.UDPAddr).Port
			diag.Close()
			if failUDP {
				ctrl.Close()
			}
			s, err := Start(t.Context(), bind, profile(), WithPorts(diagPort, ctrlPort, discPort))
			if err == nil {
				s.Close()
				t.Fatal("occupied endpoint accepted")
			}
			recovered, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: diagPort})
			if err != nil {
				t.Fatalf("diagnostic listener leaked: %v", err)
			}
			recovered.Close()
			if failUDP {
				recovered, err = net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ctrlPort})
				if err != nil {
					t.Fatalf("control listener leaked: %v", err)
				}
				recovered.Close()
			}
		})
	}
}

func TestBlockedWriteStopsOnCancelOrAliveTimeout(t *testing.T) {
	for _, alive := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancel", true: "alive-timeout"}[alive], func(t *testing.T) {
			p := profile()
			p.Responders = map[string][]byte{"22": bytes.Repeat([]byte{0x62}, 8<<20)}
			if alive {
				p.AliveLongBody = true
				p.AliveInterval = 10 * time.Millisecond
				p.AliveTimeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			s, err := Start(ctx, netip.MustParseAddr("127.0.0.1"), p, WithPorts(0, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			a, _, _ := s.Addr()
			c := dial(t, a)
			c.SetReadBuffer(1024)
			if alive {
				if read(t, c).Control != hsfz.ControlAliveCheck {
					t.Fatal("missing alive")
				}
			}
			write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22}})
			if read(t, c).Control != hsfz.ControlAck {
				t.Fatal("missing ACK")
			}
			if !alive {
				cancel()
			}
			waitStats(t, s, func(st Stats) bool { return st.ActiveConns == 0 })
		})
	}
}

func TestEmptyMultiResponderOverridesFallback(t *testing.T) {
	p := profile()
	p.Responders = map[string][]byte{"22": {0x62}}
	p.MultiResponders = map[string][]hsfz.DiagBody{"22": {}}
	s := start(t, p)
	a, _, _ := s.Addr()
	c := dial(t, a)
	write(t, c, hsfz.Frame{Control: hsfz.ControlDiag, Body: []byte{0xf4, 0x63, 0x22}})
	if read(t, c).Control != hsfz.ControlAck {
		t.Fatal("missing ACK")
	}
	c.SetReadDeadline(time.Now().Add(30 * time.Millisecond))
	if _, err := hsfz.ReadFrame(c, 100); err == nil {
		t.Fatal("empty explicit response used fallback")
	}
}
