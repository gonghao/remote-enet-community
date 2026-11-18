package integration_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/directory"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/invite"
	"remote-enet/internal/proto"
	"remote-enet/internal/secure"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/fakezgw"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
	"remote-enet/internal/tunnel"
)

func localIdentity(t fixtureT) *identity.Local {
	t.Helper()
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func secureWrap(id *identity.Local, wrappers ...func(net.Conn) net.Conn) hubtransport.Wrap {
	return func(ctx context.Context, conn net.Conn, isClient bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
		for _, wrap := range wrappers {
			conn = wrap(conn)
		}
		return secure.WrapStream(ctx, conn, secure.Opts{Local: id, IsClient: isClient, Expect: expect, Path: path})
	}
}

type transportResult struct {
	session transport.Session
	err     error
}

// Both hub arrangements use production TLS and transport adapters. Optional
// wrappers instrument the dialer's Hub connection before end-to-end TLS.
func connectPair(t fixtureT, ctx context.Context, embedded, clientDial bool, wrappers ...func(net.Conn) net.Conn) (transport.Session, transport.Session, [16]byte) {
	t.Helper()
	cid, oid := localIdentity(t), localIdentity(t)
	dialID, waitID := cid, oid
	if !clientDial {
		dialID, waitID = oid, cid
	}
	hid := waitID
	var handoff *hub.Embedded
	if embedded {
		handoff = hub.NewEmbedded()
	} else {
		hid = localIdentity(t)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hctx, stopHub := context.WithCancel(ctx)
	hubDone := make(chan error, 1)
	server := hub.NewServer(hub.Options{Local: hid, Embedded: handoff, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	go func() { hubDone <- server.Serve(hctx, ln) }()
	t.Cleanup(func() {
		stopHub()
		select {
		case err := <-hubDone:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("hub shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("hub shutdown did not join handlers")
		}
	})
	ep := transport.Endpoint{Kind: "hub", Addr: ln.Addr().String(), Hub: hid.Fingerprint()}
	token := [16]byte{1, 2, 3}
	encoded, err := (invite.Invite{Version: 1, Peer: transport.PeerInfo{ID: waitID.Fingerprint(), Endpoints: []transport.Endpoint{ep}}, Token: token, ExpiresAt: time.Now().Add(time.Hour)}).Encode()
	if err != nil {
		t.Fatal(err)
	}
	i, err := invite.Decode(encoded)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := directory.NewStatic(i.Peer).Resolve(ctx, waitID.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	_, acceptor, err := hubtransport.New(hubtransport.Options{Local: waitID, Wrap: secureWrap(waitID), Embedded: handoff, Endpoints: []transport.Endpoint{ep}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acceptor.Close() })
	connector, _, err := hubtransport.New(hubtransport.Options{Local: dialID, Wrap: secureWrap(dialID, wrappers...)})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan transportResult, 1)
	go func() { s, err := acceptor.Accept(ctx); accepted <- transportResult{s, err} }()
	var dial transport.Session
	// Registration and dialing are independent operations; only the expected
	// unknown-peer race is retried, never authentication or setup failures.
	for {
		dial, err = connector.Dial(ctx, peer)
		if !errors.Is(err, hub.ErrUnknownPeer) {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { dial.Close(nil) })
	var wait transport.Session
	select {
	case r := <-accepted:
		if r.err != nil {
			t.Fatal(r.err)
		}
		wait = r.session
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	t.Cleanup(func() { wait.Close(nil) })
	wantPath := transport.PathRelay
	if embedded {
		wantPath = transport.PathDirect
	}
	if dial.Path().Kind != wantPath || wait.Path().Kind != wantPath || dial.Peer() != waitID.Fingerprint() || wait.Peer() != dialID.Fingerprint() {
		t.Fatalf("incorrect authenticated path: dial=%+v wait=%+v", dial.Path(), wait.Path())
	}
	if extra, err := connector.Dial(ctx, peer); !errors.Is(err, hub.ErrPeerBusy) {
		if extra != nil {
			extra.Close(nil)
		}
		t.Fatalf("second session: %v", err)
	}
	if clientDial {
		return dial, wait, i.Token
	}
	return wait, dial, i.Token
}

type sessionResult struct {
	session *session.Session
	err     error
}

func runSessions(t fixtureT, ctx context.Context, client, operator transport.Session, clientOptions, operatorOptions session.Options) (*session.Session, *session.Session) {
	t.Helper()
	result := make(chan sessionResult, 1)
	go func() { s, err := session.Run(ctx, operator, operatorOptions); result <- sessionResult{s, err} }()
	c, err := session.Run(ctx, client, clientOptions)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-result:
		if r.err != nil {
			t.Fatal(r.err)
		}
		return c, r.session
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		return nil, nil
	}
}

func TestGatewayThroughHub(t *testing.T) {
	for _, embedded := range []bool{true, false} {
		for _, clientDial := range []bool{true, false} {
			t.Run(fmt.Sprintf("embedded=%t/client-dial=%t", embedded, clientDial), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				kl15 := byte(4)
				p := fakezgw.Profile{
					Ident: hsfz.Identification{DiagAddr: 0x10, MAC: [6]byte{0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}, VIN: "WBAXXXXXXX0000001"},
					KL15:  &kl15, AliveLongBody: true, AliveInterval: 10 * time.Millisecond, AliveTimeout: time.Second,
					Responders: map[string][]byte{"22f190": append([]byte{0x62, 0xf1, 0x90}, []byte("WBAXXXXXXX0000001")...)},
				}
				zgw, err := fakezgw.Start(ctx, netip.MustParseAddr("127.0.0.1"), p, fakezgw.WithPorts(0, 0, 0))
				if err != nil {
					t.Fatal(err)
				}
				defer zgw.Close()
				diag, ctrl, _ := zgw.Addr()
				ct, ot, token := connectPair(t, ctx, embedded, clientDial)
				messages := make(chan proto.VehicleState, 1)
				var pipes sync.WaitGroup
				clientOptions := session.Options{Role: session.RoleClient, Initiator: clientDial, Token: token, OnTunnel: func(ctx context.Context, h proto.OpenTunnel, st transport.Stream) {
					pipes.Add(1)
					defer pipes.Done()
					// An explicit test target set prevents this harness from dialing
					// anything other than its own ephemeral fake gateway listeners.
					if h.Target != diag.String() && h.Target != ctrl.String() {
						t.Errorf("unexpected target %q", h.Target)
						return
					}
					conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", h.Target)
					if err != nil {
						t.Error(err)
						return
					}
					_ = tunnel.Pipe(ctx, st, conn, nil)
				}}
				operatorOptions := session.Options{Role: session.RoleOperator, Initiator: !clientDial, AcceptToken: func(peer identity.Fingerprint, got [16]byte) bool {
					return peer == ot.Peer() && got == token
				}, OnMessage: func(msg any) {
					if state, ok := msg.(proto.VehicleState); ok {
						messages <- state
					}
				}}
				c, o := runSessions(t, ctx, ct, ot, clientOptions, operatorOptions)
				state := proto.VehicleState{GatewayAddr: diag.IP.String(), VIN: p.Ident.VIN, IdentRaw: p.Ident.MarshalBody(), Seq: 1}
				if err := c.Send(state); err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-messages:
					if got.VIN != state.VIN || !bytes.Equal(got.IdentRaw, state.IdentRaw) {
						t.Fatal("vehicle state changed in transit")
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				var wg sync.WaitGroup
				for range 3 {
					wg.Go(func() { exerciseDiagnostic(t, ctx, o, diag.String(), p.Responders["22f190"]) })
				}
				wg.Wait()
				st, err := o.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: ctrl.String()})
				if err != nil {
					t.Fatal(err)
				}
				defer st.Close()
				if err := hsfz.WriteFrame(st, hsfz.Frame{Control: hsfz.ControlTerminal15}); err != nil {
					t.Fatal(err)
				}
				f, err := hsfz.ReadFrame(st, 1024)
				if err != nil || f.Control != hsfz.ControlTerminal15 || !bytes.Equal(f.Body, []byte{kl15}) {
					t.Fatalf("control response=%+v err=%v", f, err)
				}
				cancel()
				for _, s := range []*session.Session{c, o} {
					select {
					case <-s.Done():
					case <-time.After(2 * time.Second):
						t.Fatal("session survived context cancellation")
					}
				}
				pipesDone := make(chan struct{})
				go func() { pipes.Wait(); close(pipesDone) }()
				select {
				case <-pipesDone:
				case <-time.After(2 * time.Second):
					t.Fatal("tunnel workers survived cancellation")
				}
			})
		}
	}
}

func exerciseDiagnostic(t *testing.T, ctx context.Context, s *session.Session, target string, response []byte) {
	t.Helper()
	st, err := s.OpenTunnel(ctx, proto.OpenTunnel{Kind: "tcp", Target: target, Origin: "integration"})
	if err != nil {
		t.Error(err)
		return
	}
	defer st.Close()
	req := hsfz.DiagBody{Src: 0xf4, Dst: 0x10, Payload: []byte{0x22, 0xf1, 0x90}}
	if err := hsfz.WriteFrame(st, hsfz.Frame{Control: hsfz.ControlDiag, Body: req.Marshal()}); err != nil {
		t.Error(err)
		return
	}
	ack, reply, alive := false, false, false
	for !ack || !reply || !alive {
		f, err := hsfz.ReadFrame(st, 1024)
		if err != nil {
			t.Error(err)
			return
		}
		switch f.Control {
		case hsfz.ControlAliveCheck:
			alive = true
			if err := hsfz.WriteFrame(st, hsfz.AliveResponse(0xf4)); err != nil {
				t.Error(err)
				return
			}
		case hsfz.ControlAck:
			ack = bytes.Equal(f.Body, req.Marshal())
			if !ack || reply {
				t.Error("invalid ACK contents or order")
				return
			}
		case hsfz.ControlDiag:
			want := (hsfz.DiagBody{Src: req.Dst, Dst: req.Src, Payload: response}).Marshal()
			reply = bytes.Equal(f.Body, want)
			if !reply || !ack {
				t.Error("invalid diagnostic response")
				return
			}
		default:
			t.Errorf("unexpected control %x", f.Control)
			return
		}
	}
}

func TestHalfCloseThroughRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ct, ot, token := connectPair(t, ctx, false, true)
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	echoDone := make(chan error, 1)
	go func() {
		conn, err := ln.AcceptTCP()
		if err != nil {
			echoDone <- err
			return
		}
		defer conn.Close()
		stop := context.AfterFunc(ctx, func() { conn.Close() })
		defer stop()
		body, err := io.ReadAll(conn)
		if err == nil {
			_, err = conn.Write(body)
		}
		if err == nil {
			err = conn.CloseWrite()
		}
		echoDone <- err
	}()
	pipeDone := make(chan error, 1)
	var stats tunnel.Stats
	c, _ := runSessions(t, ctx, ct, ot,
		session.Options{Role: session.RoleClient, Initiator: true, Token: token},
		session.Options{Role: session.RoleOperator, AcceptToken: func(_ identity.Fingerprint, got [16]byte) bool { return got == token }, OnTunnel: func(ctx context.Context, h proto.OpenTunnel, st transport.Stream) {
			if h.Target != ln.Addr().String() {
				pipeDone <- fmt.Errorf("unexpected reverse target %q", h.Target)
				return
			}
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", h.Target)
			if err == nil {
				err = tunnel.Pipe(ctx, st, conn, &stats)
			}
			pipeDone <- err
		}})
	st, err := c.OpenTunnel(ctx, proto.OpenTunnel{Kind: "reverse", Target: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	want := bytes.Repeat([]byte{0, 0xff, 1, 0x42}, 100000)
	if _, err := st.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("half-close reply: bytes=%d err=%v", len(got), err)
	}
	for _, done := range []<-chan error{echoDone, pipeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if stats.BytesIn.Load() != uint64(len(want)) || stats.BytesOut.Load() != uint64(len(want)) {
		t.Fatalf("incorrect tunnel counters: in=%d out=%d", stats.BytesIn.Load(), stats.BytesOut.Load())
	}
}

func TestPairingRejectedThroughRelay(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	ct, ot, _ := connectPair(t, ctx, false, true)
	rejected := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, ot, session.Options{Role: session.RoleOperator, AcceptToken: func(identity.Fingerprint, [16]byte) bool { return false }})
		rejected <- err
	}()
	if _, err := session.Run(ctx, ct, session.Options{Role: session.RoleClient, Initiator: true, Token: [16]byte{0xff}}); !errors.Is(err, session.ErrPairing) {
		t.Fatalf("client rejection: %v", err)
	}
	select {
	case err := <-rejected:
		if !errors.Is(err, session.ErrPairing) {
			t.Fatalf("operator rejection: %v", err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}
