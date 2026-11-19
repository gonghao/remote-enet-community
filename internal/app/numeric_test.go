package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
		{cpace.ReasonExhausted, "exhausted"},
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
