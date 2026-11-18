package exclusive

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"
)

type resolverFunc func(context.Context, netip.AddrPort, netip.AddrPort) (Identity, error)

func (f resolverFunc) Resolve(ctx context.Context, local, remote netip.AddrPort) (Identity, error) {
	return f(ctx, local, remote)
}

var (
	testLocal  = netip.MustParseAddrPort("127.0.0.1:6801")
	testRemote = netip.MustParseAddrPort("127.0.0.1:49152")
)

func TestGateOwnershipAndPIDReuse(t *testing.T) {
	owner := Identity{PID: 123, Created: 456}
	next := owner
	g := NewGate(resolverFunc(func(_ context.Context, local, remote netip.AddrPort) (Identity, error) {
		if local != testLocal || remote != testRemote {
			t.Fatalf("resolver endpoints: %v -> %v", local, remote)
		}
		return next, nil
	}))
	admit := func() *Lease {
		t.Helper()
		lease, err := g.Admit(t.Context(), testLocal, testRemote)
		if err != nil {
			t.Fatal(err)
		}
		return lease
	}
	a, b := admit(), admit()
	if got := g.Snapshot(); got != (Snapshot{Enabled: true, Owner: owner, Active: 2}) {
		t.Fatal(got)
	}
	// The PID is identical, but a later process must not inherit a live lease.
	next.Created++
	if lease, err := g.Admit(t.Context(), testLocal, testRemote); !errors.Is(err, ErrBusy) || lease != nil {
		t.Fatalf("reused PID admitted: lease=%v err=%v", lease, err)
	}
	next.PID++
	if _, err := g.Admit(t.Context(), testLocal, testRemote); !errors.Is(err, ErrBusy) {
		t.Fatalf("different PID admitted: %v", err)
	}
	a.Release()
	a.Release()
	if got := g.Snapshot(); got.Owner != owner || got.Active != 1 {
		t.Fatal(got)
	}
	b.Release()
	if got := g.Snapshot(); got != (Snapshot{Enabled: true}) {
		t.Fatal(got)
	}
	c := admit()
	if got := g.Snapshot(); got.Owner != next || got.Active != 1 {
		t.Fatal(got)
	}
	// Releasing an old lease after ownership changed must have no effect.
	b.Release()
	if got := g.Snapshot(); got.Owner != next || got.Active != 1 {
		t.Fatal(got)
	}
	c.Release()
	if got := g.Snapshot(); got != (Snapshot{Enabled: true}) {
		t.Fatal(got)
	}
}

func TestGateConcurrentAdmissionAndRelease(t *testing.T) {
	const connections = 64
	g := NewGate(resolverFunc(func(_ context.Context, _, remote netip.AddrPort) (Identity, error) {
		return Identity{PID: uint32(remote.Port()), Created: 10}, nil
	}))
	start := make(chan struct{})
	leases := make(chan *Lease, connections)
	var wg sync.WaitGroup
	for i := range connections {
		wg.Go(func() {
			<-start
			remote := netip.AddrPortFrom(testRemote.Addr(), uint16(10000+i%2))
			lease, err := g.Admit(t.Context(), testLocal, remote)
			if err == nil {
				leases <- lease
			} else if !errors.Is(err, ErrBusy) {
				t.Error(err)
			}
		})
	}
	close(start)
	wg.Wait()
	close(leases)
	if got := g.Snapshot(); got.Active != connections/2 {
		t.Fatalf("concurrent processes shared gate: %+v", got)
	}
	for lease := range leases {
		// Duplicate cleanup paths must be safe even when they race.
		wg.Go(lease.Release)
		wg.Go(lease.Release)
	}
	wg.Wait()
	if got := g.Snapshot(); got != (Snapshot{Enabled: true}) {
		t.Fatal(got)
	}
}

func TestGateUnknownAndFailedResolvers(t *testing.T) {
	queryErr := errors.New("process query denied")
	for _, tt := range []struct {
		name  string
		owner Identity
		err   error
	}{
		{name: "missing"},
		{name: "missing-pid", owner: Identity{Created: 1}},
		{name: "missing-creation", owner: Identity{PID: 1}},
		{name: "denied", err: queryErr},
		{name: "unsupported", err: ErrUnsupported},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := NewGate(resolverFunc(func(context.Context, netip.AddrPort, netip.AddrPort) (Identity, error) {
				return tt.owner, tt.err
			}))
			lease, err := g.Admit(t.Context(), testLocal, testRemote)
			if lease != nil || !errors.Is(err, ErrUnknownOwner) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("unknown owner admitted or cause lost: lease=%v err=%v", lease, err)
			}
			if got := g.Snapshot(); got != (Snapshot{Enabled: true}) {
				t.Fatal(got)
			}
		})
	}
}

func TestGateResolverBudgetAndCancellation(t *testing.T) {
	started := make(chan struct{})
	g := NewGate(resolverFunc(func(ctx context.Context, _, _ netip.AddrPort) (Identity, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > resolveTimeout {
			t.Error("resolver received no bounded deadline")
		}
		close(started)
		<-ctx.Done()
		// An injected resolver may return an identity after cancellation.
		return Identity{PID: 1, Created: 1}, nil
	}))
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		_, err := g.Admit(ctx, testLocal, testRemote)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation did not release admission")
	}
	if got := g.Snapshot(); got != (Snapshot{Enabled: true}) {
		t.Fatal(got)
	}
	// The gate also bounds resolution when the caller has no deadline.
	g = NewGate(resolverFunc(func(ctx context.Context, _, _ netip.AddrPort) (Identity, error) {
		<-ctx.Done()
		return Identity{}, ctx.Err()
	}))
	if _, err := g.Admit(t.Context(), testLocal, testRemote); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}
