package app

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"reflect"
	"sync"
	"testing"
	"time"

	"remote-enet/internal/observe"
	"remote-enet/internal/session"
)

func TestSubscriptionsCoalesceAndReturnIndependentSnapshots(t *testing.T) {
	r := New()
	slow := r.Subscribe(t.Context())
	if cap(slow) != 1 {
		t.Fatal("subscription is not bounded")
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 1000 {
			r.update(func(s *Snapshot) { s.VIN = fmt.Sprintf("vin-%d", i); s.NIC.Address = netip.MustParseAddr("127.0.0.1") })
		}
	}()
	ended(t, done)
	copied := <-slow
	if copied.VIN != "vin-999" {
		t.Fatalf("slow subscriber received stale state: %+v", copied)
	}
	copied.VIN, copied.NIC.Name = "changed", "changed"
	if s := r.Snapshot(); s.VIN != "vin-999" || s.NIC.Name == "changed" {
		t.Fatal("snapshot aliases runtime storage")
	}
	failure := errors.New("latest error")
	r.finish(failure)
	stopped := <-slow
	if stopped.Phase != PhaseStopped || stopped.LastError != "Runtime stopped with an error. Check runtime diagnostics." {
		t.Fatalf("missing final snapshot: %+v", stopped)
	}
	if _, ok := <-slow; ok {
		t.Fatal("subscription survived runtime exit")
	}
	late := r.Subscribe(t.Context())
	if got := <-late; !sameSnapshot(got, stopped) {
		t.Fatalf("late subscription = %+v, want %+v", got, stopped)
	}
	if _, ok := <-late; ok {
		t.Fatal("late subscription not closed")
	}
}

func TestSubscriptionCancellationAndStopAreConcurrent(t *testing.T) {
	r := New()
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				ctx, cancel := context.WithCancel(t.Context())
				ch := r.Subscribe(ctx)
				_ = r.Snapshot()
				cancel()
				for range ch {
				}
			}
		}()
	}
	writes := make(chan struct{})
	go func() {
		defer close(writes)
		for range 500 {
			r.update(func(s *Snapshot) { s.Generation++ })
		}
		r.finish(nil)
	}()
	ended(t, writes)
	wg.Wait()
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.subs) != 0 {
		t.Fatal("canceled subscriptions retained")
	}
}

func TestRuntimeRejectsReuseAndInvalidConfig(t *testing.T) {
	r := New()
	events := r.Subscribe(t.Context())
	err := r.RunClient(t.Context(), ClientConfig{})
	if err == nil {
		t.Fatal("invalid config accepted")
	}
	stopped := snapshotWhere(t, events, func(s Snapshot) bool { return s.Phase == PhaseStopped })
	if stopped.Role != session.RoleClient || stopped.LastError == "" {
		t.Fatalf("invalid failure snapshot: %+v", stopped)
	}
	if err := r.RunOperator(t.Context(), OperatorConfig{}); !errors.Is(err, ErrAlreadyRun) {
		t.Fatalf("reused runtime: %v", err)
	}
	if !sameSnapshot(r.Snapshot(), stopped) {
		t.Fatal("reused runtime mutated completed state")
	}
}

func TestPauseHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	started := time.Now()
	if err := pause(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if time.Since(started) > time.Second {
		t.Fatal("pause ignored cancellation")
	}
}

func sameSnapshot(a, b Snapshot) bool {
	a.Metrics, b.Metrics = observe.Snapshot{}, observe.Snapshot{}
	return reflect.DeepEqual(a, b)
}
