package hub

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	router "remote-enet/internal/hub"
	"remote-enet/internal/identity"
	"remote-enet/internal/transport"
)

var errRefresh = errors.New("transport/hub: wait refreshed")

type waitAttempt struct {
	cancel context.CancelCauseFunc
}

// Refresh invalidates outstanding registrations, including a ticket being
// signed or awaiting retry. The first inner read removes a wait from this set.
func (a *Connector) Refresh() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for w := range a.idle {
		w.cancel(errRefresh)
	}
}

func (a *Connector) wait(ctx context.Context, ep transport.Endpoint) (transport.Session, error) {
	delay := 100 * time.Millisecond
	for ctx.Err() == nil {
		attemptCtx, cancel := context.WithCancelCause(ctx)
		w := &waitAttempt{cancel: cancel}
		a.mu.Lock()
		a.idle[w] = struct{}{}
		a.mu.Unlock()
		retire := func() {
			a.mu.Lock()
			delete(a.idle, w)
			a.mu.Unlock()
		}
		conn, err := a.registerWait(attemptCtx, ep)
		if err == nil {
			var s transport.Session
			s, err = a.wrap(attemptCtx, conn, false, identity.Fingerprint{}, path(conn, ep, identity.Fingerprint{}), retire)
			retire()
			if s != nil {
				return ownSession(attemptCtx, s, func() { cancel(context.Canceled) }), nil
			}
			if !errors.Is(context.Cause(attemptCtx), errRefresh) {
				cancel(err)
				return nil, err
			}
		} else {
			if errors.Is(err, identity.ErrFingerprintMismatch) {
				retire()
				cancel(err)
				return nil, err
			}
			backoff := delay
			if errors.Is(err, router.ErrUnauthorized) {
				backoff = 30 * time.Second
			} else {
				delay = min(delay*2, 5*time.Second)
			}
			select {
			case <-attemptCtx.Done():
			case <-time.After(backoff):
			}
			retire()
		}
		if errors.Is(context.Cause(attemptCtx), errRefresh) {
			delay = 100 * time.Millisecond
		}
		cancel(context.Canceled)
	}
	return nil, ctx.Err()
}

func (a *Connector) registerWait(ctx context.Context, ep transport.Endpoint) (conn net.Conn, err error) {
	req := router.Request{Role: router.RoleWait}
	if notify := a.o.OnRegister; notify != nil {
		defer func() { go notify(ep.Hub, bytes.Clone(req.Token), err) }()
	}
	if a.o.WaitToken != nil {
		token, err := a.o.WaitToken(ctx, ep.Hub)
		if err != nil {
			return nil, fmt.Errorf("transport/hub: sign wait ticket: %w", err)
		}
		req.Token = token
	}
	return router.Connect(ctx, ep.Addr, a.o.Local, ep.Hub, req)
}
