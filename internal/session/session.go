// Package session runs the versioned control protocol over one transport.
package session

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/pairing/cpace"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

type Role string

const (
	RoleClient   Role = "client"
	RoleOperator Role = "operator"
)

var (
	ErrVersion = errors.New("session: incompatible protocol version")
	ErrPairing = errors.New("session: pairing rejected")
)

const handshakeTimeout = 10 * time.Second

// CenterBinding comes from local center configuration and the pinned outer connection.
type CenterBinding struct {
	CenterID string
	Session  [16]byte
	Hub      identity.Fingerprint
}

type Options struct {
	// Center requires ConfirmSAS and starts with the tunnel gate closed.
	Center *CenterBinding
	// Pinned checks the client fingerprint within this center session.
	Pinned      func(identity.Fingerprint) bool
	HoldTunnels bool
	// ConfirmTimeout defaults to three minutes in center mode.
	ConfirmTimeout time.Duration
	// BeginClientAttempt charges the center client's budget immediately before frame 11. False means exhausted.
	BeginClientAttempt func() bool
	// Initiator is true on the transport dialer, independent of Role.
	Initiator    bool
	Role         Role
	AppVersion   string
	Capabilities []string
	// AllowLinkTest advertises and accepts peer-only tests; the zero value disables them.
	AllowLinkTest bool
	Trusted       func(identity.Fingerprint) bool
	// AcceptToken owns atomic token consumption and persistence of trust.
	AcceptToken    func(identity.Fingerprint, [16]byte) bool
	Token          [16]byte
	DigitalCode    string
	DigitalManager DigitalResponder
	// ConfirmSAS may block until ctx is canceled. A nil hook accepts automatically outside center mode.
	ConfirmSAS func(context.Context, string, identity.Fingerprint) (bool, error)
	LocalFP    identity.Fingerprint
	// HubFP selects hub-bound PAKE. Use zero for direct pairing.
	HubFP identity.Fingerprint
	// EndpointHub is the Client's expected Hub identity for device tickets.
	EndpointHub identity.Fingerprint
	// RequireHubTicket prevents a Client using a bearer invitation from pairing
	// with an Operator that cannot replace it with a device ticket.
	RequireHubTicket bool
	// GrantTicket enables Operator ticket delivery and must return promptly.
	GrantTicket func(client identity.Fingerprint) ([]byte, error)
	// Now supplies the Client clock for ticket expiry. Nil uses time.Now.
	Now      func() time.Time
	Remember func(context.Context, identity.Fingerprint) error
	// Callbacks must return when ctx is canceled. OnMessage must not block.
	OnTunnel  func(context.Context, proto.OpenTunnel, transport.Stream)
	OnMessage func(any)
	// OnRTT receives each validated Pong once, outside pingMu. It must not
	// block or perform I/O; no goroutine is created for a sample.
	OnRTT        func(time.Duration)
	PingInterval time.Duration
	Logger       *slog.Logger
}
type Session struct {
	ts      transport.Session
	control transport.Stream
	opts    Options
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	err     error
	writeMu sync.Mutex
	rtt     atomic.Int64
	pingMu  sync.Mutex
	seq     uint64
	sent    time.Time
	// NegotiatedCapabilities is immutable after Run returns.
	NegotiatedCapabilities []string
	setupTimer             *time.Timer
	outcome                PairingOutcome
	hubTicket              []byte
	pendingTicket          []byte
	linkTestNegotiated     bool
	tunnelsHeld            atomic.Bool
	linkTestIn             atomic.Bool
	linkTestOut            atomic.Bool
}

// Run reserves the first stream for control: Initiator opens it, the other
// endpoint accepts it. No other streams may precede it.
// The caller's context owns the entire lifetime, including returned tunnels.
func Run(ctx context.Context, ts transport.Session, o Options) (*Session, error) {
	if o.Role != RoleClient && o.Role != RoleOperator {
		return nil, fmt.Errorf("session: invalid role %q", o.Role)
	}
	if o.Center != nil {
		if o.ConfirmSAS == nil || o.ConfirmTimeout < 0 {
			return nil, errors.New("session: center mode requires SAS confirmation and a nonnegative timeout")
		}
		if o.ConfirmTimeout == 0 {
			o.ConfirmTimeout = 3 * time.Minute
		}
	}
	if o.PingInterval < 0 {
		return nil, fmt.Errorf("session: negative ping interval")
	}
	if o.PingInterval == 0 {
		o.PingInterval = time.Second
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	o.Capabilities = linkTestCapabilities(o.Capabilities, o.AllowLinkTest)
	o.Capabilities = slices.DeleteFunc(o.Capabilities, func(v string) bool {
		return v == proto.PairDirectCapability || v == proto.PairHubCapability || v == proto.HubTicketCapability || v == proto.PairCenterCapability
	})
	if o.Role == RoleClient && o.DigitalCode != "" || o.Role == RoleOperator && (o.DigitalManager != nil || o.Center != nil) {
		o.Capabilities = append(o.Capabilities, o.pairCapability())
	}
	if o.Center == nil && (o.Role == RoleClient || o.GrantTicket != nil) {
		o.Capabilities = append(o.Capabilities, proto.HubTicketCapability)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	runCtx, cancel := context.WithCancel(ctx)
	s := &Session{ts: ts, opts: o, ctx: runCtx, cancel: cancel, done: make(chan struct{})}
	s.tunnelsHeld.Store(o.HoldTunnels || o.Center != nil)
	transportDone := ts.Done()
	if o.Center != nil && o.Role == RoleClient && o.DigitalCode == "" {
		// The control reader owns the final buffered not_paired even if the
		// transport has already signaled EOF.
		transportDone = nil
	}
	go func() {
		select {
		case <-ctx.Done():
			s.finish(ctx.Err())
		case <-transportDone:
			s.finish(ts.Err())
		case <-s.done:
		}
	}()
	// Transport shutdown also cancels runCtx. The handshake budget must depend
	// only on the caller, or teardown can replace a decoded protocol rejection
	// with context.Canceled before handshake returns.
	hctx, hcancel := context.WithCancelCause(ctx)
	s.setupTimer = time.AfterFunc(handshakeTimeout, func() { hcancel(context.DeadlineExceeded) })
	stop := context.AfterFunc(hctx, func() { s.finish(context.Cause(hctx)) })
	err := s.handshake(hctx)
	s.opts.DigitalCode = ""
	s.setupTimer.Stop()
	s.setupTimer = nil
	stop()
	handshakeContextErr := context.Cause(hctx)
	protocolErr := errors.Is(err, ErrPairing) || errors.Is(err, ErrVersion)
	if !protocolErr {
		if handshakeContextErr != nil {
			err = handshakeContextErr
		} else if transportErr := ts.Err(); err != nil && transportErr != nil && !errors.Is(err, transportErr) {
			err = fmt.Errorf("%w: %w", transportErr, err)
		}
	}
	hcancel(context.Canceled)
	if err != nil {
		if o.Role == RoleClient && o.DigitalCode != "" && !protocolErr {
			// Hello and transport errors can contain unauthenticated peer text.
			err = pairingFailure(ReasonInterrupted)
			if handshakeContextErr != nil {
				err = fmt.Errorf("%w: %w", err, handshakeContextErr)
			}
		}
		s.finish(err)
		return nil, fmt.Errorf("session: handshake: %w", err)
	}
	if o.Center != nil && s.outcome == DigitalPaired {
		// The peer may close immediately after its decision. The verified outcome
		// survives that close, including while the client finishes its local check.
		go s.acceptLoop()
		return s, nil
	}
	select {
	case <-s.done:
		return nil, s.Err()
	default:
	}
	go s.readControl()
	go s.pingLoop()
	go s.acceptLoop()
	return s, nil
}

const invalidPairingToken = "invalid pairing token"

func (s *Session) handshake(ctx context.Context) error {
	var err error
	if s.opts.Initiator {
		s.control, err = s.ts.OpenStream(ctx)
	} else {
		s.control, err = s.ts.AcceptStream(ctx)
	}
	if err != nil {
		return err
	}
	local := proto.Hello{ProtoVersion: proto.Version, Role: string(s.opts.Role), AppVersion: s.opts.AppVersion, Capabilities: s.opts.Capabilities}
	localHash, peerHash := sha256.New(), sha256.New()
	writer := io.MultiWriter(s.control, localHash)
	reader := io.TeeReader(s.control, peerHash)
	var msg any
	if s.opts.Initiator {
		if err = proto.Write(writer, local); err != nil {
			return err
		}
		msg, err = proto.Read(reader, proto.MaxFrameLen)
	} else {
		msg, err = proto.Read(reader, proto.MaxFrameLen)
		if err == nil {
			err = proto.Write(writer, local)
			// A peer can consume our Hello and close before Write completes.
			if peer, ok := msg.(proto.Hello); err != nil && ok && peer.ProtoVersion == proto.Version && s.opts.unsupportedPairing(peer) {
				return pairingFailure(ReasonUnsupported)
			}
		}
	}
	if err != nil {
		return err
	}
	hello, ok := msg.(proto.Hello)
	if !ok {
		return fmt.Errorf("session: expected Hello, got %T", msg)
	}
	if hello.ProtoVersion != proto.Version {
		return fmt.Errorf("%w: peer %d, local %d", ErrVersion, hello.ProtoVersion, proto.Version)
	}
	if (s.opts.Role == RoleClient && hello.Role != string(RoleOperator)) || (s.opts.Role == RoleOperator && hello.Role != string(RoleClient)) {
		return errors.New("session: unexpected peer role")
	}
	for _, cap := range s.opts.Capabilities {
		for _, remote := range hello.Capabilities {
			if cap == remote {
				s.NegotiatedCapabilities = append(s.NegotiatedCapabilities, cap)
				if cap == linktest.Capability {
					s.linkTestNegotiated = true
				}
				break
			}
		}
	}
	if err := s.authorize(ctx, hello, [32]byte(localHash.Sum(nil)), [32]byte(peerHash.Sum(nil))); err != nil {
		return err
	}
	if slices.Contains(s.NegotiatedCapabilities, proto.HubTicketCapability) {
		return s.exchangeHubTicket()
	}
	return nil
}

func (o Options) unsupportedPairing(peer proto.Hello) bool {
	if o.Role == RoleClient {
		return peer.Role == string(RoleOperator) && o.DigitalCode != "" && !slices.Contains(peer.Capabilities, o.pairCapability())
	}
	requested := slices.Contains(peer.Capabilities, proto.PairCenterCapability) || slices.Contains(peer.Capabilities, proto.PairDirectCapability) || slices.Contains(peer.Capabilities, proto.PairHubCapability)
	return peer.Role == string(RoleClient) && requested && ((o.Center == nil && o.DigitalManager == nil) || !slices.Contains(peer.Capabilities, o.pairCapability()))
}

func (s *Session) authorize(ctx context.Context, hello proto.Hello, localHash, peerHash [32]byte) error {
	if s.opts.Center == nil && s.opts.Role == RoleClient && s.opts.RequireHubTicket && !slices.Contains(s.NegotiatedCapabilities, proto.HubTicketCapability) {
		return pairingFailure(ReasonHubTicket)
	}
	if s.opts.Center != nil && s.opts.Role == RoleOperator && (slices.Contains(hello.Capabilities, proto.PairDirectCapability) || slices.Contains(hello.Capabilities, proto.PairHubCapability)) {
		return pairingFailure(ReasonUnsupported)
	}
	if slices.Contains(s.NegotiatedCapabilities, s.opts.pairCapability()) {
		if err := s.digital(ctx, localHash, peerHash); err != nil {
			return err
		}
		s.outcome = DigitalPaired
		return nil
	}
	if s.opts.unsupportedPairing(hello) {
		if !s.opts.Initiator {
			s.finishRejection()
		}
		return pairingFailure(ReasonUnsupported)
	}
	if s.opts.Center != nil {
		if s.opts.Role == RoleOperator && (s.opts.Pinned == nil || !s.opts.Pinned(s.Peer())) {
			return s.rejectPair(cpace.ReasonNotPaired)
		}
		return nil
	}
	if s.opts.Role == RoleClient {
		if s.opts.Token != [16]byte{} {
			if err := proto.Write(s.control, proto.PairToken{Token: s.opts.Token}); err != nil {
				return err
			}
		}
		msg, err := proto.Read(s.control, proto.MaxFrameLen)
		if err != nil {
			return err
		}
		result, ok := msg.(proto.PairResult)
		if !ok {
			return fmt.Errorf("session: expected PairResult, got %T", msg)
		}
		if !result.Accepted {
			if result.Reason == string(ReasonHubTicket) && slices.Contains(s.NegotiatedCapabilities, proto.HubTicketCapability) {
				return pairingFailure(ReasonHubTicket)
			}
			if result.Reason == invalidPairingToken {
				return fmt.Errorf("%w: %s", ErrPairing, invalidPairingToken)
			}
			return ErrPairing
		}
		return nil
	}
	trusted := s.opts.Trusted != nil && s.opts.Trusted(s.Peer())
	var token proto.PairToken
	if !trusted {
		msg, err := proto.Read(s.control, proto.MaxFrameLen)
		if err != nil {
			return err
		}
		token, _ = msg.(proto.PairToken)
	}
	if trusted || (token.Token != [16]byte{} && s.opts.AcceptToken != nil) {
		if err := s.prepareHubTicket(); err != nil {
			if proto.Write(s.control, proto.PairResult{Reason: string(ReasonHubTicket)}) == nil {
				s.finishRejection()
			}
			return err
		}
		if !trusted {
			trusted = s.opts.AcceptToken(s.Peer(), token.Token)
		}
	}
	result := proto.PairResult{Accepted: trusted}
	if !trusted {
		result.Reason = invalidPairingToken
	}
	if err := proto.Write(s.control, result); err != nil {
		if !trusted {
			return fmt.Errorf("%w: send rejection: %w", ErrPairing, err)
		}
		return err
	}
	if !trusted {
		s.finishRejection()
		return ErrPairing
	}
	return nil
}

// The request's yamux Write can still be waiting for completion after we have
// read it. Keep the mux alive until the peer consumes the rejection and closes.
func (s *Session) finishRejection() {
	// A manual SAS check may have extended the setup timer beyond the I/O budget.
	if s.opts.ConfirmSAS != nil && s.setupTimer.Stop() {
		s.setupTimer.Reset(handshakeTimeout)
	}
	if err := s.control.CloseWrite(); err == nil {
		// Run closes the transport on either setup timeout or caller cancellation.
		_, _ = io.Copy(io.Discard, s.control)
	}
}

func (s *Session) finish(err error) {
	if err == nil {
		err = io.EOF
	}
	s.once.Do(func() {
		if transportErr := s.ts.Err(); transportErr != nil && !errors.Is(err, cpace.ReasonNotPaired) {
			err = transportErr
		}
		s.mu.Lock()
		s.err = err
		s.mu.Unlock()
		s.cancel()
		_ = s.ts.Close(err)
		close(s.done)
	})
}

// Send serializes complete frames. Writes fail when the session terminates.
func (s *Session) Send(msg any) error {
	if s.opts.Center != nil && s.outcome == DigitalPaired {
		return errors.New("session: center pairing exchange complete")
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if err := proto.Write(s.control, msg); err != nil {
		s.finishControlIO(err)
		return err
	}
	return nil
}

// A failed accept or write can race the final buffered decision on a data client.
func (s *Session) finishControlIO(err error) {
	if s.opts.Center != nil && s.opts.Role == RoleClient && s.outcome != DigitalPaired {
		return
	}
	s.finish(err)
}

func (s *Session) readControl() {
	read := func(r io.Reader) (any, error) { return proto.Read(r, proto.MaxFrameLen) }
	if s.opts.Center != nil {
		read = proto.ReadCenterControl
	}
	for {
		msg, err := read(s.control)
		if err != nil {
			s.finish(err)
			return
		}
		switch m := msg.(type) {
		case proto.Unknown:
			s.opts.Logger.Debug("ignoring unknown control message", "type", m.Type)
		case proto.Ping:
			if err := s.Send(proto.Pong{Seq: m.Seq, SentAt: m.SentAt}); err != nil && s.ctx.Err() != nil {
				return
			}
		case proto.Pong:
			var sample time.Duration
			valid := false
			s.pingMu.Lock()
			if m.Seq == s.seq && !s.sent.IsZero() && m.SentAt == s.sent.UnixNano() {
				sample = time.Since(s.sent)
				valid = true
				s.rtt.Store(int64(sample))
				s.sent = time.Time{}
			}
			s.pingMu.Unlock()
			if valid && s.opts.OnRTT != nil {
				s.opts.OnRTT(sample)
			}
		case proto.PairToken: // A trusted peer may still send an invitation token.
		case proto.PairDecision:
			if s.opts.Center != nil && s.opts.Role == RoleClient && !m.Accepted && m.Reason == string(cpace.ReasonNotPaired) {
				s.finish(pairingFailure(cpace.ReasonNotPaired))
			} else {
				s.finish(fmt.Errorf("session: unexpected control message %T", m))
			}
			return
		case proto.Hello, proto.HubTicket, proto.PairResult, proto.OpenTunnel, proto.PakeCommit, proto.PakeShareA, proto.PakeShareB, proto.PakeConfirmA, proto.PakeConfirmB:
			s.finish(fmt.Errorf("session: unexpected control message %T", m))
			return
		default:
			if s.opts.OnMessage != nil {
				s.opts.OnMessage(msg)
			}
		}
	}
}
func (s *Session) pingLoop() {
	tick := time.NewTicker(s.opts.PingInterval)
	defer tick.Stop()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-tick.C:
			s.pingMu.Lock()
			if !s.sent.IsZero() {
				s.pingMu.Unlock()
				continue
			}
			s.seq++
			s.sent = time.Now()
			p := proto.Ping{Seq: s.seq, SentAt: s.sent.UnixNano()}
			s.pingMu.Unlock()
			if err := s.Send(p); err != nil {
				return
			}
		}
	}
}
func (s *Session) acceptLoop() {
	for {
		st, err := s.ts.AcceptStream(s.ctx)
		if err != nil {
			s.finishControlIO(err)
			return
		}
		if s.tunnelsHeld.Load() {
			_ = st.Close()
			continue
		}
		go s.dispatch(st)
	}
}
func (s *Session) dispatch(st transport.Stream) {
	defer st.Close()
	stop := context.AfterFunc(s.ctx, func() { _ = st.Close() })
	defer stop()
	timer := time.AfterFunc(handshakeTimeout, func() { _ = st.Close() })
	msg, err := proto.Read(st, proto.MaxFrameLen)
	if !timer.Stop() || err != nil {
		return
	}
	hdr, ok := msg.(proto.OpenTunnel)
	if !ok {
		return
	}
	if hdr.Kind == linktest.Capability {
		if s.linkTestNegotiated && hdr.Target == "" && hdr.Origin == "" {
			s.serveLinkTest(st)
		}
		return
	}
	if !s.validTunnel(hdr, false) || s.opts.OnTunnel == nil {
		return
	}
	s.opts.OnTunnel(s.ctx, hdr, st)
}
func (s *Session) validTunnel(h proto.OpenTunnel, outbound bool) bool {
	role := s.opts.Role
	if !outbound {
		if role == RoleClient {
			role = RoleOperator
		} else {
			role = RoleClient
		}
	}
	return h.Target != "" && ((role == RoleOperator && h.Kind == "tcp") || (role == RoleClient && h.Kind == "reverse"))
}

// ErrTunnelsHeld signals that center authorization has not released this connection.
var ErrTunnelsHeld = errors.New("session: tunnels held until the center confirms the session")

// ReleaseTunnels is idempotent. A center pairing connection never carries tunnels.
func (s *Session) ReleaseTunnels() {
	if s.opts.Center == nil || s.outcome != DigitalPaired {
		s.tunnelsHeld.Store(false)
	}
}

// OpenTunnel writes only the header; subsequent stream bytes are opaque.
func (s *Session) OpenTunnel(ctx context.Context, hdr proto.OpenTunnel) (transport.Stream, error) {
	if s.tunnelsHeld.Load() {
		return nil, ErrTunnelsHeld
	}
	if !s.validTunnel(hdr, true) {
		return nil, fmt.Errorf("session: invalid tunnel kind %q for role %q", hdr.Kind, s.opts.Role)
	}
	return s.openTunnel(ctx, hdr)
}

func (s *Session) openTunnel(ctx context.Context, hdr proto.OpenTunnel) (transport.Stream, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	opCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopSession := context.AfterFunc(s.ctx, cancel)
	defer stopSession()
	st, err := s.ts.OpenStream(opCtx)
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(opCtx, func() { _ = st.Close() })
	err = proto.Write(st, hdr)
	stopped := stop()
	if !stopped || opCtx.Err() != nil {
		_ = st.Close()
		return nil, opCtx.Err()
	}
	if err != nil {
		_ = st.Close()
		return nil, err
	}
	return st, nil
}
func (s *Session) RTT() time.Duration         { return time.Duration(s.rtt.Load()) }
func (s *Session) Peer() identity.Fingerprint { return s.ts.Peer() }
func (s *Session) Done() <-chan struct{}      { return s.done }
func (s *Session) Err() error {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.err
	default:
		return nil
	}
}
