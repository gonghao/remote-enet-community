package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"remote-enet/internal/linktest"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

func linkTestCapabilities(capabilities []string, allow bool) []string {
	result := make([]string, 0, len(capabilities)+1)
	for _, capability := range capabilities {
		// A manually supplied capability cannot bypass the explicit opt-in.
		if capability != linktest.Capability {
			result = append(result, capability)
		}
	}
	if allow {
		result = append(result, linktest.Capability)
	}
	return result
}

// TestLink terminates at the authenticated peer and never invokes OnTunnel.
// At most one outbound and one inbound test may run concurrently per session.
// Unsupported peers return linktest.ErrUnsupported without opening a stream.
func (s *Session) TestLink(ctx context.Context, o linktest.Options) (result linktest.Result, err error) {
	started := time.Now()
	defer func() {
		result.Duration = time.Since(started)
		result.TimedOut = errors.Is(err, context.DeadlineExceeded)
		result.Interrupted = err != nil && !result.TimedOut
	}()
	if !s.linkTestNegotiated {
		return result, linktest.ErrUnsupported
	}
	o, err = o.Normalize()
	if err != nil {
		return result, err
	}
	result.RequestedSamples = o.Samples
	if !s.linkTestOut.CompareAndSwap(false, true) {
		return result, linktest.ErrBusy
	}
	defer s.linkTestOut.Store(false)
	opCtx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	st, err := s.openTunnel(opCtx, proto.OpenTunnel{Kind: linktest.Capability})
	if err != nil {
		if opCtx.Err() != nil {
			return result, opCtx.Err()
		}
		return result, fmt.Errorf("%w: open stream: %w", linktest.ErrTransport, err)
	}
	return linktest.Run(opCtx, st, o)
}

func (s *Session) serveLinkTest(st transport.Stream) {
	if !s.linkTestIn.CompareAndSwap(false, true) {
		return
	}
	defer s.linkTestIn.Store(false)
	if err := linktest.Serve(s.ctx, st); err != nil && s.ctx.Err() == nil {
		s.opts.Logger.Debug("link test ended", "error", err)
	}
}
