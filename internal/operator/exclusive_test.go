package operator

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
	"remote-enet/internal/transport"
)

type resolveFunc func(context.Context, netip.AddrPort, netip.AddrPort) (exclusive.Identity, error)

func (f resolveFunc) Resolve(ctx context.Context, local, remote netip.AddrPort) (exclusive.Identity, error) {
	return f(ctx, local, remote)
}

type readSignalStream struct {
	transport.Stream
	started chan<- struct{}
	once    sync.Once
}

func (s *readSignalStream) Read(b []byte) (int, error) {
	s.once.Do(func() { s.started <- struct{}{} })
	return s.Stream.Read(b)
}

type resolvedOwner struct {
	owner exclusive.Identity
	err   error
}

type ownerQuery struct {
	local, remote netip.AddrPort
	reply         chan resolvedOwner
}

func controlledResolver() (exclusive.Resolver, <-chan ownerQuery) {
	queries := make(chan ownerQuery, 16)
	return resolveFunc(func(ctx context.Context, local, remote netip.AddrPort) (exclusive.Identity, error) {
		query := ownerQuery{local: local, remote: remote, reply: make(chan resolvedOwner, 1)}
		select {
		case queries <- query:
		case <-ctx.Done():
			return exclusive.Identity{}, ctx.Err()
		}
		select {
		case result := <-query.reply:
			return result.owner, result.err
		case <-ctx.Done():
			return exclusive.Identity{}, ctx.Err()
		}
	}), queries
}

func nextOwnerQuery(t *testing.T, queries <-chan ownerQuery) ownerQuery {
	t.Helper()
	select {
	case query := <-queries:
		return query
	case <-time.After(time.Second):
		t.Fatal("owner resolution did not start")
		return ownerQuery{}
	}
}

func resolveConnection(t *testing.T, queries <-chan ownerQuery, c *net.TCPConn, owner exclusive.Identity) {
	t.Helper()
	q := nextOwnerQuery(t, queries)
	if q.local != c.RemoteAddr().(*net.TCPAddr).AddrPort() || q.remote != c.LocalAddr().(*net.TCPAddr).AddrPort() {
		t.Fatalf("accepted endpoints swapped or changed: %+v, client %v -> %v", q, c.LocalAddr(), c.RemoteAddr())
	}
	q.reply <- resolvedOwner{owner: owner}
}

func waitExclusive(t *testing.T, p *Presenter, want exclusive.Snapshot) {
	t.Helper()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if got := p.ExclusiveState(); got == want {
			return
		}
		select {
		case <-tick.C:
		case <-timer.C:
			t.Fatalf("exclusive state=%+v, want %+v", p.ExclusiveState(), want)
		}
	}
}

func expectRejected(t *testing.T, c *net.TCPConn) {
	t.Helper()
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	if err == nil || (errors.As(err, &ne) && ne.Timeout()) {
		t.Fatalf("connection not promptly rejected: %v", err)
	}
}

func noTunnelRequest(t *testing.T, requests <-chan proto.OpenTunnel) {
	t.Helper()
	select {
	case request := <-requests:
		t.Fatalf("rejected connection opened a tunnel: %+v", request)
	default:
	}
}

func TestExclusiveOwnerAcrossPortsAndUpdate(t *testing.T) {
	resolver, queries := controlledResolver()
	opener, requests, _ := newEchoOpener(t)
	p := startPresenter(t, Options{Exclusive: true, Resolver: resolver, Opener: opener, State: testState(t, "127.0.0.2")})
	diag, ctrl, _ := p.Addr()
	a := exclusive.Identity{PID: 100, Created: 200}
	b := exclusive.Identity{PID: 101, Created: 201}
	c1 := dialTCP(t, diag)
	resolveConnection(t, queries, c1, a)
	nextRequest(t, requests)
	c2 := dialTCP(t, ctrl)
	resolveConnection(t, queries, c2, a)
	nextRequest(t, requests)
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: a, Active: 2})
	for _, owner := range []exclusive.Identity{b, {PID: a.PID, Created: a.Created + 1}, {}} {
		c := dialTCP(t, ctrl)
		resolveConnection(t, queries, c, owner)
		expectRejected(t, c)
		noTunnelRequest(t, requests)
	}
	if err := p.Update(testState(t, "127.0.0.3")); err != nil {
		t.Fatal(err)
	}
	roundTrip(t, c1, []byte("diagnostic"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: a, Active: 1})
	c := dialTCP(t, diag)
	resolveConnection(t, queries, c, b)
	expectRejected(t, c)
	noTunnelRequest(t, requests)
	roundTrip(t, c2, []byte("control"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
	c = dialTCP(t, diag)
	resolveConnection(t, queries, c, b)
	if h := nextRequest(t, requests); h.Target != "127.0.0.3:6801" {
		t.Fatal(h)
	}
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: b, Active: 1})
	roundTrip(t, c, []byte("next owner"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
}

func TestExclusiveConcurrentPortsChooseOneOwner(t *testing.T) {
	resolver, queries := controlledResolver()
	opener, requests, _ := newEchoOpener(t)
	p := startPresenter(t, Options{Exclusive: true, Resolver: resolver, Opener: opener, State: testState(t, "127.0.0.2")})
	diag, ctrl, _ := p.Addr()
	d, c := dialTCP(t, diag), dialTCP(t, ctrl)
	q1, q2 := nextOwnerQuery(t, queries), nextOwnerQuery(t, queries)
	ownerFor := func(q ownerQuery) exclusive.Identity {
		return exclusive.Identity{PID: uint32(q.local.Port()), Created: 100}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	for _, q := range []ownerQuery{q1, q2} {
		wg.Go(func() {
			<-start
			q.reply <- resolvedOwner{owner: ownerFor(q)}
		})
	}
	close(start)
	wg.Wait()
	h := nextRequest(t, requests)
	winner, loser := d, c
	if h.Origin == c.LocalAddr().String() {
		winner, loser = c, d
	} else if h.Origin != d.LocalAddr().String() {
		t.Fatal(h)
	}
	expectRejected(t, loser)
	noTunnelRequest(t, requests)
	owner := exclusive.Identity{PID: uint32(winner.RemoteAddr().(*net.TCPAddr).Port), Created: 100}
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: owner, Active: 1})
	roundTrip(t, winner, []byte("one owner"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
}

func TestExclusiveResolutionAndOpenFailureRelease(t *testing.T) {
	resolver, queries := controlledResolver()
	requests := make(chan proto.OpenTunnel, 8)
	p := startPresenter(t, Options{Exclusive: true, Resolver: resolver, State: testState(t, "127.0.0.2"),
		Opener: openFunc(func(_ context.Context, h proto.OpenTunnel) (transport.Stream, error) {
			requests <- h
			return nil, errors.New("tunnel unavailable")
		})})
	diag, ctrl, _ := p.Addr()
	for _, err := range []error{exclusive.ErrUnknownOwner, exclusive.ErrUnsupported, errors.New("access denied")} {
		c := dialTCP(t, diag)
		nextOwnerQuery(t, queries).reply <- resolvedOwner{err: err}
		expectRejected(t, c)
		noTunnelRequest(t, requests)
		waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
	}
	for i, addr := range []*net.TCPAddr{diag, ctrl} {
		c := dialTCP(t, addr)
		resolveConnection(t, queries, c, exclusive.Identity{PID: uint32(i + 1), Created: 1})
		nextRequest(t, requests)
		expectRejected(t, c)
		waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
	}
}

func TestExclusiveDisabledDoesNotResolve(t *testing.T) {
	resolver := resolveFunc(func(context.Context, netip.AddrPort, netip.AddrPort) (exclusive.Identity, error) {
		t.Error("disabled exclusive mode queried an owner")
		return exclusive.Identity{}, errors.New("must not resolve")
	})
	opener, requests, _ := newEchoOpener(t)
	p := startPresenter(t, Options{Resolver: resolver, Opener: opener, State: testState(t, "127.0.0.2")})
	diag, ctrl, _ := p.Addr()
	d, c := dialTCP(t, diag), dialTCP(t, ctrl)
	nextRequest(t, requests)
	nextRequest(t, requests)
	if got := p.ExclusiveState(); got != (exclusive.Snapshot{}) {
		t.Fatal(got)
	}
	roundTrip(t, d, []byte("diagnostic"))
	roundTrip(t, c, []byte("control"))
}

func TestExclusiveHalfCloseRetainsOwner(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	local, remote := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	readDone, reply := make(chan struct{}), make(chan struct{})
	worker := make(chan struct{})
	go func() {
		defer close(worker)
		st, err := remote.AcceptStream(ctx)
		if err != nil {
			return
		}
		defer st.Close()
		body, err := io.ReadAll(st)
		if err != nil {
			return
		}
		close(readDone)
		select {
		case <-reply:
		case <-ctx.Done():
			return
		}
		if _, err := st.Write(body); err == nil {
			_ = st.CloseWrite()
		}
	}()
	t.Cleanup(func() { cancel(); local.Close(nil); remote.Close(nil); <-worker })
	resolver, queries := controlledResolver()
	p := startPresenter(t, Options{Exclusive: true, Resolver: resolver, State: testState(t, "127.0.0.2"),
		Opener: openFunc(func(ctx context.Context, _ proto.OpenTunnel) (transport.Stream, error) {
			return local.OpenStream(ctx)
		})})
	diag, ctrl, _ := p.Addr()
	a := exclusive.Identity{PID: 1, Created: 2}
	c := dialTCP(t, diag)
	resolveConnection(t, queries, c, a)
	want := []byte("half-close preserves the read side")
	if _, err := c.Write(want); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive EOF")
	}
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: a, Active: 1})
	other := dialTCP(t, ctrl)
	resolveConnection(t, queries, other, exclusive.Identity{PID: 2, Created: 3})
	expectRejected(t, other)
	close(reply)
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("half-close reply=%q err=%v", got, err)
	}
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
}

func TestExclusiveShutdownReleasesEachStage(t *testing.T) {
	for _, stage := range []string{"resolve", "open", "pipe"} {
		for _, shutdown := range []string{"cancel", "close"} {
			t.Run(stage+"/"+shutdown, func(t *testing.T) {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				started := make(chan struct{}, 1)
				resolver := resolveFunc(func(ctx context.Context, _, _ netip.AddrPort) (exclusive.Identity, error) {
					if stage == "resolve" {
						started <- struct{}{}
						<-ctx.Done()
						return exclusive.Identity{}, ctx.Err()
					}
					return exclusive.Identity{PID: 1, Created: 2}, nil
				})
				echo, _, _ := newEchoOpener(t)
				opener := openFunc(func(ctx context.Context, h proto.OpenTunnel) (transport.Stream, error) {
					if stage != "pipe" {
						started <- struct{}{}
						<-ctx.Done()
						return nil, ctx.Err()
					}
					stream, err := echo.OpenTunnel(ctx, h)
					if err != nil {
						return nil, err
					}
					return &readSignalStream{Stream: stream, started: started}, nil
				})
				p, err := Start(ctx, Options{Bind: netip.MustParseAddr("127.0.0.1"), Exclusive: true, Resolver: resolver,
					Opener: opener, State: testState(t, "127.0.0.2"), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { p.Close() })
				diag, _, _ := p.Addr()
				c := dialTCP(t, diag)
				select {
				case <-started:
				case <-time.After(time.Second):
					t.Fatal("worker did not start")
				}
				if stage != "resolve" {
					waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: exclusive.Identity{PID: 1, Created: 2}, Active: 1})
				}
				if shutdown == "cancel" {
					cancel()
				}
				closed := make(chan struct{})
				go func() { p.Close(); close(closed) }()
				select {
				case <-closed:
				case <-time.After(2 * time.Second):
					t.Fatal("Close did not join workers")
				}
				expectRejected(t, c)
				if got := p.ExclusiveState(); got != (exclusive.Snapshot{Enabled: true}) {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestInjectedGateSharesExternalOwnership(t *testing.T) {
	resolver, queries := controlledResolver()
	gate := exclusive.NewGate(resolver)
	opener, requests, _ := newEchoOpener(t)
	p := startPresenter(t, Options{Exclusive: true, Gate: gate, Opener: opener, State: testState(t, "127.0.0.2")})
	owner := exclusive.Identity{PID: 100, Created: 200}
	leaseResult := make(chan *exclusive.Lease, 1)
	go func() {
		lease, err := gate.Admit(t.Context(), netip.MustParseAddrPort("10.99.0.8:22"), netip.MustParseAddrPort("192.0.2.1:12345"))
		if err != nil {
			t.Error(err)
		}
		leaseResult <- lease
	}()
	q := nextOwnerQuery(t, queries)
	q.reply <- resolvedOwner{owner: owner}
	lease := <-leaseResult
	defer lease.Release()
	if err := p.WithMaintenance(t.Context(), func(context.Context, netip.Addr) error { t.Fatal("maintenance bypassed external lease"); return nil }); !errors.Is(err, exclusive.ErrBusy) {
		t.Fatal(err)
	}
	diag, _, _ := p.Addr()
	other := dialTCP(t, diag)
	resolveConnection(t, queries, other, exclusive.Identity{PID: 101, Created: 201})
	expectRejected(t, other)
	noTunnelRequest(t, requests)
	same := dialTCP(t, diag)
	resolveConnection(t, queries, same, owner)
	nextRequest(t, requests)
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: owner, Active: 2})
	roundTrip(t, same, []byte("shared owner"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: owner, Active: 1})
}
