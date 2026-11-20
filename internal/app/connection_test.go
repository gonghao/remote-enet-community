package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"remote-enet/internal/carside"
	"remote-enet/internal/hsfz"
	"remote-enet/internal/identity"
	"remote-enet/internal/operator"
	"remote-enet/internal/proto"
	"remote-enet/internal/session"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
	"remote-enet/internal/trust"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func connectionConfig(nic carside.NIC) ClientConfig {
	return ClientConfig{NICName: nic.Name, NICCheckInterval: time.Hour, Logger: quietLogger(), Hooks: ClientHooks{
		Interfaces: func(context.Context) ([]carside.NIC, error) { return []carside.NIC{nic}, nil },
		Wait:       pause,
	}}
}

type connectionResult struct {
	connected bool
	fatal     bool
	err       error
}

func result(t *testing.T, ch <-chan connectionResult) connectionResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(2 * time.Second):
		t.Fatal("connection did not terminate")
		return connectionResult{}
	}
}

func storeForTest(t *testing.T) (*trust.Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := trust.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	return s, dir
}

func tokenForTest(t *testing.T, s *trust.Store) [16]byte {
	t.Helper()
	v, err := s.Issue(t.Context(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func gatewayState(t *testing.T) (carside.NIC, carside.Gateway) {
	t.Helper()
	id := hsfz.Identification{DiagAddr: 0x10, VIN: "WBAXXXXXXX0000001", MAC: [6]byte{1, 2, 3, 4, 5, 6}}
	raw, err := (hsfz.Frame{Control: hsfz.ControlVehicleIdent, Body: id.MarshalBody()}).MarshalBinary()
	if err != nil {
		t.Fatal(err)
	}
	return carside.NIC{Name: "test", Address: netip.MustParseAddr("127.0.0.1"), Prefix: netip.MustParsePrefix("127.0.0.0/8")}, carside.Gateway{Address: netip.MustParseAddr("127.0.0.2"), Identification: id, IdentRaw: raw}
}

func TestOperatorRejectsTokenAndCorruptAuthorization(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "bad-token", true: "corrupt-store"}[corrupt], func(t *testing.T) {
			store, dir := storeForTest(t)
			if corrupt {
				if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
			done := make(chan connectionResult, 1)
			go func() {
				fatal, err := New().operatorConnection(ctx, b, OperatorConfig{}, store, quietLogger())
				done <- connectionResult{fatal: fatal, err: err}
			}()
			if _, err := session.Run(ctx, a, session.Options{Role: session.RoleClient, Initiator: true, Token: [16]byte{9}}); err == nil {
				t.Fatal("unauthorized client accepted")
			}
			r := result(t, done)
			if corrupt {
				if !r.fatal || !errors.Is(r.err, trust.ErrCorrupt) {
					t.Fatalf("corrupt auth result: fatal=%t err=%v", r.fatal, r.err)
				}
			} else if r.fatal || !errors.Is(r.err, session.ErrPairing) {
				t.Fatalf("bad token result: fatal=%t err=%v", r.fatal, r.err)
			}
		})
	}
}

func TestTrustedClientCanReuseConsumedInvitation(t *testing.T) {
	store, _ := storeForTest(t)
	token := tokenForTest(t, store)
	peer := identity.Fingerprint{1}
	if accepted, err := store.Consume(t.Context(), peer, token); !accepted || err != nil {
		t.Fatalf("initial pairing: accepted=%t err=%v", accepted, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	a, b := memtransport.Pair(t, peer, identity.Fingerprint{2})
	done := make(chan connectionResult, 1)
	go func() {
		fatal, err := New().operatorConnection(ctx, b, OperatorConfig{}, store, quietLogger())
		done <- connectionResult{fatal: fatal, err: err}
	}()
	// A client may still present its original invitation after recovering its
	// peer file. Trust is checked before token consumption, using its device ID.
	if _, err := session.Run(ctx, a, session.Options{Role: session.RoleClient, Initiator: true, Token: token}); err != nil {
		t.Fatalf("trusted client rejected its consumed invitation: %v", err)
	}
	cancel()
	if r := result(t, done); r.fatal {
		t.Fatalf("trusted connection became fatal: %+v", r)
	}
	if accepted, err := store.Consume(t.Context(), identity.Fingerprint{3}, token); accepted || err != nil {
		t.Fatalf("consumed invitation admitted another identity: %t %v", accepted, err)
	}
}

func TestOperatorInvalidStateIsLocalButBindFailureIsFatal(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(map[bool]string{false: "invalid-state", true: "occupied-port"}[occupied], func(t *testing.T) {
			store, _ := storeForTest(t)
			token := tokenForTest(t, store)
			cfg := OperatorConfig{Bind: netip.MustParseAddr("127.0.0.1")}
			if occupied {
				ln, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
				if err != nil {
					t.Fatal(err)
				}
				defer ln.Close()
				cfg.Ports = operator.Ports{Diagnostic: ln.Addr().(*net.TCPAddr).Port}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
			done := make(chan connectionResult, 1)
			go func() {
				fatal, err := New().operatorConnection(ctx, b, cfg, store, quietLogger())
				done <- connectionResult{fatal: fatal, err: err}
			}()
			client, err := session.Run(ctx, a, session.Options{Role: session.RoleClient, Initiator: true, Token: token})
			if err != nil {
				t.Fatal(err)
			}
			nic, gateway := gatewayState(t)
			state := gateway.VehicleState(nic, 1)
			if !occupied {
				state.IdentRaw = []byte("invalid")
			}
			// Rejection may close yamux after consuming the frame but before
			// the sender's Write returns. Assert the receiver's exact cause.
			sendErr := client.Send(state)
			r := result(t, done)
			if r.err == nil || r.fatal != occupied {
				t.Fatalf("unexpected result: %+v", r)
			}
			prefix := "invalid initial vehicle state:"
			if occupied {
				prefix = "start diagnostic listeners:"
			}
			if !strings.HasPrefix(r.err.Error(), prefix) {
				t.Fatalf("wrong rejection cause: %v (send=%v)", r.err, sendErr)
			}
			if sendErr != nil && !errors.Is(sendErr, yamux.ErrSessionShutdown) && !errors.Is(sendErr, yamux.ErrStreamClosed) && !errors.Is(sendErr, io.ErrClosedPipe) && !errors.Is(sendErr, net.ErrClosed) {
				t.Fatalf("unexpected send failure: %v", sendErr)
			}
			select {
			case <-a.Done():
			case <-time.After(time.Second):
				t.Fatal("rejected session survived")
			}
		})
	}
}

type testConnector struct{ session transport.Session }

func (testConnector) DialBootstrap(context.Context, string) (transport.Session, identity.Fingerprint, error) {
	panic("unexpected bootstrap dial on a pinned connection")
}

func (c testConnector) Dial(context.Context, transport.PeerInfo) (transport.Session, error) {
	return c.session, nil
}

func TestClientPersistsPeerAndFailsClosedOnSaveError(t *testing.T) {
	for _, corrupt := range []bool{false, true} {
		t.Run(map[bool]string{false: "save", true: "corrupt-store"}[corrupt], func(t *testing.T) {
			store, dir := storeForTest(t)
			if corrupt {
				if err := os.WriteFile(filepath.Join(dir, "trust.json"), []byte("{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
			states := make(chan proto.VehicleState, 1)
			server := make(chan *session.Session, 1)
			errs := make(chan error, 1)
			go func() {
				s, err := session.Run(ctx, b, session.Options{Role: session.RoleOperator, Trusted: func(identity.Fingerprint) bool { return true }, OnMessage: func(msg any) {
					if state, ok := msg.(proto.VehicleState); ok {
						states <- state
					}
				}})
				if err != nil {
					errs <- err
					return
				}
				server <- s
			}()
			peer := transport.PeerInfo{ID: identity.Fingerprint{2}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{2}}}}
			nic, gateway := gatewayState(t)
			done := make(chan connectionResult, 1)
			go func() {
				_, connected, fatal, err := New().clientConnection(ctx, testConnector{a}, peer, [16]byte{}, store, nic, gateway, connectionConfig(nic))
				done <- connectionResult{connected, fatal, err}
			}()
			var s *session.Session
			select {
			case s = <-server:
			case err := <-errs:
				if !corrupt {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("server handshake did not finish")
			}
			if corrupt {
				r := result(t, done)
				if !r.connected || !r.fatal || !errors.Is(r.err, trust.ErrCorrupt) {
					t.Fatalf("save failure: %+v", r)
				}
				return
			}
			select {
			case state := <-states:
				if state.GatewayAddr != gateway.Address.String() || !bytes.Equal(state.IdentRaw, gateway.IdentRaw) {
					t.Fatal("client changed vehicle state")
				}
			case <-time.After(time.Second):
				t.Fatal("vehicle state missing")
			}
			got, err := store.Peer(ctx, peer.ID)
			if err != nil || got.Endpoints[0] != peer.Endpoints[0] {
				t.Fatalf("saved peer: %+v %v", got, err)
			}
			b.Close(errors.New("test disconnect"))
			r := result(t, done)
			if !r.connected || r.fatal || r.err == nil {
				t.Fatalf("disconnect result: %+v", r)
			}
			select {
			case <-s.Done():
			case <-time.After(time.Second):
				t.Fatal("server survived client cleanup")
			}
		})
	}
}

type clientConnectedHandler struct {
	slog.Handler
	connected chan struct{}
	resume    chan struct{}
}

func (h clientConnectedHandler) Handle(ctx context.Context, record slog.Record) error {
	if record.Message == "client connected" {
		close(h.connected)
		<-h.resume
	}
	return h.Handler.Handle(ctx, record)
}

func TestClientRecordsNICChangeWhenSessionAlreadyEnded(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	store, _ := storeForTest(t)
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	server := make(chan error, 1)
	go func() {
		_, err := session.Run(ctx, b, session.Options{
			Role: session.RoleOperator, Trusted: func(identity.Fingerprint) bool { return true },
		})
		server <- err
	}()
	nic, gateway := gatewayState(t)
	checkNIC := make(chan struct{})
	cfg := connectionConfig(nic)
	cfg.Hooks.Wait = func(ctx context.Context, _ time.Duration) error {
		select {
		case <-checkNIC:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	cfg.Hooks.Interfaces = func(context.Context) ([]carside.NIC, error) {
		changed := nic
		changed.Address = netip.MustParseAddr("127.0.0.2")
		return []carside.NIC{changed}, nil
	}
	handler := clientConnectedHandler{
		Handler: quietLogger().Handler(), connected: make(chan struct{}), resume: make(chan struct{}, 1),
	}
	defer close(handler.resume)
	cfg.Logger = slog.New(handler)
	peer := transport.PeerInfo{ID: identity.Fingerprint{2}, Endpoints: []transport.Endpoint{{Kind: "hub", Addr: "localhost:47000", Hub: identity.Fingerprint{2}}}}
	r := New()
	done := make(chan connectionResult, 1)
	go func() {
		_, connected, fatal, err := r.clientConnection(ctx, testConnector{a}, peer, [16]byte{}, store, nic, gateway, cfg)
		done <- connectionResult{connected, fatal, err}
	}()
	if err := <-server; err != nil {
		t.Fatal(err)
	}
	select {
	case <-handler.connected:
	case <-ctx.Done():
		t.Fatal("client did not connect")
	}
	r.mu.Lock()
	active := r.active
	r.mu.Unlock()
	close(checkNIC)
	select {
	case <-active.s.Done():
	case <-ctx.Done():
		t.Fatal("session did not end after NIC change")
	}
	if !errors.Is(active.s.Err(), context.Canceled) || !errors.Is(context.Cause(active.ctx), ErrNICChanged) {
		t.Fatalf("expected both end signals, got session=%v connection=%v", active.s.Err(), context.Cause(active.ctx))
	}
	handler.resume <- struct{}{}
	if got := result(t, done); !got.connected || got.fatal || !errors.Is(got.err, ErrNICChanged) {
		t.Fatalf("connection result: %+v", got)
	}
	for _, event := range r.Snapshot().Metrics.Events {
		if event.Kind == "session-end" {
			if event.Error != ErrNICChanged.Error() {
				t.Fatalf("recorded session end: %q", event.Error)
			}
			return
		}
	}
	t.Fatal("missing session-end event")
}
