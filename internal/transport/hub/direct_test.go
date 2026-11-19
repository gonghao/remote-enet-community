package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/secure"
	"remote-enet/internal/transport"
)

func TestDialAfterClose(t *testing.T) {
	local := id(t)
	connector, acceptor, err := New(Options{
		Local: local, Embedded: router.NewEmbedded(), Wrap: stubWrap(local.Fingerprint()),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := acceptor.Close(); err != nil {
		t.Fatal(err)
	}
	if session, err := connector.Dial(t.Context(), transport.PeerInfo{ID: local.Fingerprint()}); session != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Dial after Close: %v", err)
	}
	if session, err := connector.DialDirectPair(t.Context(), "127.0.0.1:0"); session != nil || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("DialDirectPair after Close: %v", err)
	}
}

func TestDirectPairInnerExporter(t *testing.T) {
	for _, name := range []string{"explicit-close", "context-cancel", "different-inner-device"} {
		mismatch := name == "different-inner-device"
		t.Run(name, func(t *testing.T) {
			operator, client := id(t), id(t)
			inner := operator
			if mismatch {
				inner = id(t)
			}
			handoff := router.NewEmbedded()
			ep, listener := startCountedHub(t, operator, handoff)
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			finished := make(chan struct{})
			defer func() { cancel(); <-finished }()
			var want [32]byte
			peerDone := make(chan error, 1)
			go func() {
				defer close(finished)
				conn, peer, err := handoff.Accept(ctx)
				if err != nil {
					peerDone <- err
					return
				}
				defer conn.Close()
				if peer != client.Fingerprint() {
					peerDone <- errors.New("wrong outer client identity")
					return
				}
				tc := tls.Server(conn, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{inner.Cert}, NextProtos: []string{"renet/1"}, ClientAuth: tls.RequireAnyClientCert, SessionTicketsDisabled: true})
				if err := tc.HandshakeContext(ctx); err != nil {
					peerDone <- err
					return
				}
				state := tc.ConnectionState()
				data, err := state.ExportKeyingMaterial("EXPORTER-renet-pair-direct-v1", nil, 32)
				if err == nil {
					want = [32]byte(data)
					_, err = tc.Write([]byte{1})
				}
				peerDone <- err
				<-ctx.Done()
			}()
			wraps := 0
			c, _, err := New(Options{Local: client, Token: []byte("standalone"), Wrap: func(ctx context.Context, conn net.Conn, clientRole bool, expect identity.Fingerprint, path transport.PathInfo) (transport.Session, error) {
				wraps++
				return secure.WrapStream(ctx, conn, secure.Opts{Local: client, Expect: expect, IsClient: clientRole, Path: path})
			}})
			if err != nil {
				t.Fatal(err)
			}
			session, err := c.DialDirectPair(ctx, ep.Addr)
			peerErr := <-peerDone
			if listener.count.Load() != 1 || wraps != 1 {
				t.Fatalf("outer connections %d, inner wraps %d", listener.count.Load(), wraps)
			}
			if mismatch {
				if err == nil || session != nil {
					t.Fatal("accepted another inner identity")
				}
				return
			}
			if err != nil || peerErr != nil {
				t.Fatalf("client %v, peer %v", err, peerErr)
			}
			if session.Peer() != operator.Fingerprint() || session.Path().Kind != transport.PathDirect {
				t.Fatal("wrong peer or path")
			}
			got := session.(interface{ PairingExporter() [32]byte }).PairingExporter()
			if got != want {
				t.Fatal("wrapped session exporter differs from inner TLS peer")
			}
			if name == "context-cancel" {
				cancel()
				select {
				case <-session.Done():
				case <-time.After(time.Second):
					t.Fatal("cancellation did not finish session cleanup")
				}
				return
			}
			reason := errors.New("direct session closed")
			if err := session.Close(reason); err != nil {
				t.Fatal(err)
			}
			select {
			case <-session.Done():
			default:
				t.Fatal("Close returned before Done")
			}
			if !errors.Is(session.Err(), reason) {
				t.Fatal(session.Err())
			}
		})
	}
}
