package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/operator"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/fakezgw"
)

func TestApplicationGateway(t *testing.T) {
	type gatewayCase struct{ embedded, halfClose, broadcast bool }
	cases := []gatewayCase{{true, false, false}, {false, false, false}, {true, true, false}}
	if runtime.GOOS == "linux" {
		cases = append(cases, gatewayCase{embedded: true, broadcast: true})
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("embedded=%t/half-close=%t/broadcast=%t", tc.embedded, tc.halfClose, tc.broadcast), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			nic, err := carside.SelectNIC("", netip.MustParseAddr("127.0.0.1"))
			if err != nil {
				t.Fatal(err)
			}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			kl15 := byte(4)
			response := []byte{0x62, 0xf1, 0x90, 1, 2, 3}
			profile := fakezgw.Profile{
				Ident:      hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, VIN: "WBAXXXXXXX0000001"},
				Responders: map[string][]byte{"22f190": response}, KL15: &kl15,
				AliveLongBody: true, AliveInterval: 15 * time.Millisecond, AliveTimeout: time.Second,
			}
			if tc.halfClose {
				profile.AliveInterval = 0
			}
			zgw, err := fakezgw.Start(ctx, netip.MustParseAddr("127.0.0.2"), profile, fakezgw.WithPorts(0, 0, 0))
			if err != nil {
				t.Fatal(err)
			}
			defer zgw.Close()
			diag, ctrl, disc := zgw.Addr()
			gateways, err := carside.Discover(ctx, carside.DiscoverOptions{NIC: nic, Destinations: []netip.AddrPort{disc.AddrPort()}, Timeout: 30 * time.Millisecond})
			if err != nil || len(gateways) != 1 {
				t.Fatalf("discovery: gateways=%d err=%v", len(gateways), err)
			}
			forwarder, err := carside.NewForwarder(nic, logger)
			if err != nil {
				t.Fatal(err)
			}
			defer forwarder.Close()
			ct, ot, token := connectPair(t, ctx, tc.embedded, true)
			states := make(chan proto.VehicleState, 1)
			c, o := runSessions(t, ctx, ct, ot,
				session.Options{Role: session.RoleClient, Initiator: true, Token: token, OnTunnel: forwarder.HandleTunnel},
				session.Options{Role: session.RoleOperator, AcceptToken: func(_ identity.Fingerprint, got [16]byte) bool { return got == token }, OnMessage: func(m any) {
					if state, ok := m.(proto.VehicleState); ok {
						states <- state
					}
				}})
			if err := c.Send(gateways[0].VehicleState(nic, 1)); err != nil {
				t.Fatal(err)
			}
			var state proto.VehicleState
			select {
			case state = <-states:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			p, err := operator.Start(ctx, operator.Options{Bind: nic.Address, State: state, RemoteDiagnosticPort: uint16(diag.Port), RemoteControlPort: uint16(ctrl.Port), Opener: o, Logger: logger})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			localDiag, localCtrl, localDisc := p.Addr()
			assertDiscoveryReplay(t, localDisc, gateways[0].IdentRaw, tc.broadcast)
			var wg sync.WaitGroup
			for range 3 {
				wg.Go(func() { diagnosticOverSocket(t, localDiag, response, !tc.halfClose, tc.halfClose) })
			}
			wg.Wait()
			control, err := net.DialTCP("tcp4", nil, localCtrl)
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			_ = control.SetDeadline(time.Now().Add(2 * time.Second))
			if err := hsfz.WriteFrame(control, hsfz.Frame{Control: hsfz.ControlTerminal15}); err != nil {
				t.Fatal(err)
			}
			f, err := hsfz.ReadFrame(control, 1024)
			if err != nil || f.Control != hsfz.ControlTerminal15 || !bytes.Equal(f.Body, []byte{kl15}) {
				t.Fatalf("control response=%+v err=%v", f, err)
			}
			// The tunnel handler must reject the selected machine itself even
			// though its address lies in the selected car-side subnet.
			blocked, err := o.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: localDiag.String()})
			if err != nil {
				t.Fatal(err)
			}
			defer blocked.Close()
			closed := make(chan error, 1)
			go func() { _, err := blocked.Read(make([]byte, 1)); closed <- err }()
			select {
			case err := <-closed:
				if err == nil {
					t.Fatal("forbidden tunnel remained open")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("forbidden target was not rejected")
			}
			cancel()
			if err := p.Close(); err != nil {
				t.Fatal(err)
			}
			if err := forwarder.Close(); err != nil {
				t.Fatal(err)
			}
			assertPortsReleased(t, localDiag, localCtrl, localDisc)
		})
	}
}

func assertDiscoveryReplay(t *testing.T, addr *net.UDPAddr, want []byte, broadcast bool) {
	t.Helper()
	tool, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()
	_ = tool.SetDeadline(time.Now().Add(2 * time.Second))
	target := *addr
	if broadcast {
		target.IP = net.IPv4(127, 255, 255, 255)
	}
	if _, err := tool.WriteToUDP(hsfz.IdentRequest, &target); err != nil {
		t.Fatal(err)
	}
	var buf [65535]byte
	n, from, err := tool.ReadFromUDP(buf[:])
	if err != nil || !from.IP.Equal(addr.IP) || from.Port != addr.Port || !bytes.Equal(buf[:n], want) {
		t.Fatalf("discovery replay: from=%v bytes=%d err=%v", from, n, err)
	}
}

func diagnosticOverSocket(t *testing.T, addr *net.TCPAddr, response []byte, wantAlive, halfClose bool) {
	t.Helper()
	tool, err := net.DialTCP("tcp4", nil, addr)
	if err != nil {
		t.Error(err)
		return
	}
	defer tool.Close()
	_ = tool.SetDeadline(time.Now().Add(2 * time.Second))
	req := hsfz.DiagBody{Src: 0xf4, Dst: 0x10, Payload: []byte{0x22, 0xf1, 0x90}}
	if err := hsfz.WriteFrame(tool, hsfz.Frame{Control: hsfz.ControlDiag, Body: req.Marshal()}); err != nil {
		t.Error(err)
		return
	}
	if halfClose {
		if err := tool.CloseWrite(); err != nil {
			t.Error(err)
			return
		}
	}
	ack, reply, alive := false, false, !wantAlive
	for !ack || !reply || !alive {
		f, err := hsfz.ReadFrame(tool, 1024)
		if err != nil {
			t.Error(err)
			return
		}
		switch f.Control {
		case hsfz.ControlAck:
			if reply || !bytes.Equal(f.Body, req.Marshal()) {
				t.Error("invalid ACK contents or order")
				return
			}
			ack = true
		case hsfz.ControlDiag:
			want := (hsfz.DiagBody{Src: req.Dst, Dst: req.Src, Payload: response}).Marshal()
			if !ack || !bytes.Equal(f.Body, want) {
				t.Error("invalid diagnostic response")
				return
			}
			reply = true
		case hsfz.ControlAliveCheck:
			if err := hsfz.WriteFrame(tool, hsfz.AliveResponse(req.Src)); err != nil {
				t.Error(err)
				return
			}
			alive = true
		default:
			t.Errorf("unexpected control %x", f.Control)
			return
		}
	}
	if halfClose {
		if _, err := tool.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("half-close EOF: %v", err)
		}
	}
}

func assertPortsReleased(t *testing.T, diag, ctrl *net.TCPAddr, disc *net.UDPAddr) {
	t.Helper()
	for _, addr := range []*net.TCPAddr{diag, ctrl} {
		listener, err := net.ListenTCP("tcp4", addr)
		if err != nil {
			t.Fatalf("listener not released: %s: %v", addr, err)
		}
		listener.Close()
	}
	socket, err := net.ListenUDP("udp4", disc)
	if err != nil {
		t.Fatalf("discovery socket not released: %s: %v", disc, err)
	}
	socket.Close()
}
