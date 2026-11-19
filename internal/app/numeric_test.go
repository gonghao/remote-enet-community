package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"remote-enet/internal/carside"
	"remote-enet/internal/identity"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/session"
	"remote-enet/internal/transport"
	hubtransport "remote-enet/internal/transport/hub"
)

type mismatchConnector struct {
	dialFunc
	calls int
}

func (c *mismatchConnector) DialDirectPair(context.Context, string) (transport.Session, error) {
	c.calls++
	return nil, identity.ErrFingerprintMismatch
}

func TestDigitalIdentityMismatchTerminatesFirstDial(t *testing.T) {
	nic, gateway := gatewayState(t)
	cfg := connectionConfig(nic)
	cfg.DirectPair = &DirectPair{Address: "127.0.0.1:47000", Code: "00001234"}
	cfg.Hooks.Discover = func(context.Context, carside.DiscoverOptions) ([]carside.Gateway, error) {
		return []carside.Gateway{gateway}, nil
	}
	cfg.Hooks.Wait = func(ctx context.Context, delay time.Duration) error {
		if delay != cfg.NICCheckInterval {
			t.Error("digital pairing entered retry backoff")
		}
		return pause(ctx, delay)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	connector := &mismatchConnector{}
	err := New().clientLoop(ctx, cfg, connector, transport.PeerInfo{}, [16]byte{}, nil)
	if !errors.Is(err, PairingErrorKind("identity_mismatch")) || connector.calls != 1 {
		t.Fatalf("first dial: error=%v, attempts=%d", err, connector.calls)
	}
}

func TestPairingErrorClassification(t *testing.T) {
	const secret = "01234567"
	for _, tc := range []struct {
		err  error
		want PairingErrorKind
	}{
		{&net.DNSError{Name: secret, Err: "no such host"}, "dns"},
		{context.DeadlineExceeded, "timeout"},
		{hubtransport.ErrPeerBusy, "busy"},
		{identity.ErrFingerprintMismatch, "identity_mismatch"},
		{session.ReasonUnsupported, "unsupported"},
		{cpace.ReasonStoreFailed, "store_failed"},
		{cpace.ReasonRateLimited, "rate_limited"},
		{cpace.ReasonBudgetExhausted, "budget_exhausted"},
		{cpace.Reason(secret), "interrupted"},
		{io.ErrUnexpectedEOF, "interrupted"},
		{context.Canceled, "interrupted"},
	} {
		err := classifyPairingError(fmt.Errorf("%s: %w", secret, tc.err))
		if !errors.Is(err, tc.want) || err.Error() != string(tc.want) {
			t.Errorf("wrong classification for %T", tc.err)
		}
	}
}

func TestOperatorDoesNotPublishUnpairedClient(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	addr := freeTCPAddress(t)
	nic, _ := gatewayState(t)
	var logs bytes.Buffer
	cfg := OperatorConfig{StateDir: t.TempDir(), Listen: addr, ShareAddress: addr, Bind: nic.Address,
		Logger: slog.New(slog.NewTextHandler(&logs, nil))}
	op := New()
	states := op.Subscribe(ctx)
	done := make(chan error, 1)
	go func() { done <- op.RunOperator(ctx, cfg) }()
	defer func() {
		cancel()
		if err := awaitError(t, done); !errors.Is(err, context.Canceled) {
			t.Errorf("operator stop: %v", err)
		}
	}()
	snapshotWhere(t, states, func(s Snapshot) bool { return s.Phase == PhaseListening })
	display, err := op.IssueNumericCode(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wrong := "00000000"
	if display.Code == wrong {
		wrong = "00000001"
	}
	id, err := identity.Generate()
	if err != nil {
		t.Fatal(err)
	}
	connector, _, err := hubtransport.New(hubtransport.Options{Local: id, Wrap: wrap(id)})
	if err != nil {
		t.Fatal(err)
	}
	ts, err := connector.DialDirectPair(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer ts.Close(nil)
	checkPeer := func(s Snapshot) bool {
		if !s.Peer.IsZero() {
			t.Fatalf("phase %s exposed unpaired client %s", s.Phase, s.Peer)
		}
		return s.Phase == PhaseConnecting
	}
	snapshotWhere(t, states, checkPeer)
	if _, err := session.Run(ctx, ts, session.Options{
		Role: session.RoleClient, Initiator: true, DigitalCode: wrong, LocalFP: id.Fingerprint(),
	}); !errors.Is(err, cpace.ReasonConfirmation) {
		t.Fatalf("wrong code: %v", err)
	}
	ended := snapshotWhere(t, states, func(s Snapshot) bool {
		checkPeer(s)
		return s.Phase == PhaseListening
	})
	if ended.LastError != "confirmation" {
		t.Errorf("pairing reason: %q", ended.LastError)
	}
	for line := range strings.SplitSeq(logs.String(), "\n") {
		if strings.Contains(line, "operator session ended") {
			if strings.Contains(line, "peer=") || strings.Contains(line, id.Fingerprint().String()) {
				t.Fatal("session-end log disclosed an unpaired client")
			}
			return
		}
	}
	t.Fatal("unpaired session did not log its end")
}
