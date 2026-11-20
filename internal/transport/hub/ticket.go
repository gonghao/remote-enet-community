package hub

import (
	"context"
	"errors"
	"fmt"
	"net"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

// ErrUnknownPeer and ErrUnauthorized are shared with the outer protocol.
var (
	ErrUnknownPeer  = router.ErrUnknownPeer
	ErrUnauthorized = router.ErrUnauthorized
)

// ErrHubFingerprintMismatch identifies a failed outer pin, before inner Wrap.
// Such errors also match identity.ErrFingerprintMismatch.
var ErrHubFingerprintMismatch = errors.New("transport/hub: hub fingerprint mismatch")

// WaitTicket registers once with the supplied token at ep.Addr, pinned to
// ep.Hub. The same target pins the inner peer. ctx owns the wait and session.
func (a *Connector) WaitTicket(ctx context.Context, ep transport.Endpoint, target identity.Fingerprint, token []byte) (transport.Session, error) {
	return a.connectTicket(ctx, ep, router.Request{Role: router.RoleWait, Target: target, Token: token})
}

// DialTicket dials once with the supplied token at ep.Addr, pinned to ep.Hub.
// The same peer pins the dial target and the inner peer. ctx owns the session.
func (a *Connector) DialTicket(ctx context.Context, ep transport.Endpoint, peer identity.Fingerprint, token []byte) (transport.Session, error) {
	return a.connectTicket(ctx, ep, router.Request{Role: router.RoleDial, Target: peer, Token: token})
}

func (a *Connector) connectTicket(ctx context.Context, ep transport.Endpoint, req router.Request) (transport.Session, error) {
	if a.isClosed() {
		return nil, net.ErrClosed
	}
	if req.Target.IsZero() {
		return nil, errors.New("transport/hub: peer identity required")
	}
	conn, err := router.Connect(ctx, ep.Addr, a.o.Local, ep.Hub, req)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, cause
		}
		if errors.Is(err, identity.ErrFingerprintMismatch) {
			return nil, fmt.Errorf("%w: %w", ErrHubFingerprintMismatch, err)
		}
		return nil, fmt.Errorf("transport/hub: connect with ticket: %w", err)
	}
	return a.wrap(ctx, conn, req.Role == router.RoleDial, req.Target, path(conn, ep, req.Target), innerSetup{waitForTraffic: req.Role == router.RoleWait})
}
