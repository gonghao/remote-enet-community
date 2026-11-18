package session

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"remote-enet/internal/identity"
	"remote-enet/internal/proto"
	"remote-enet/internal/testutil/memtransport"
)

func TestOnRTTReceivesOnlyValidatedPongsOutsideLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	a, b := memtransport.Pair(t, identity.Fingerprint{1}, identity.Fingerprint{2})
	samples := make(chan time.Duration, 4)
	marker := make(chan struct{}, 1)
	var current atomic.Pointer[Session]
	ready := make(chan error, 1)
	go func() {
		s, err := Run(ctx, a, Options{Initiator: true, Role: RoleClient, PingInterval: 5 * time.Millisecond,
			OnRTT: func(sample time.Duration) {
				if s := current.Load(); s != nil {
					s.pingMu.Lock()
					s.pingMu.Unlock()
				}
				select {
				case samples <- sample:
				default:
				}
			}, OnMessage: func(any) { marker <- struct{}{} }})
		current.Store(s)
		ready <- err
	}()
	control, err := b.AcceptStream(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = proto.Read(control, proto.MaxFrameLen); err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(control, proto.Hello{ProtoVersion: proto.Version, Role: string(RoleOperator)}); err != nil {
		t.Fatal(err)
	}
	if err = proto.Write(control, proto.PairResult{Accepted: true}); err != nil {
		t.Fatal(err)
	}
	if err = <-ready; err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		msg, err := proto.Read(control, proto.MaxFrameLen)
		if err != nil {
			t.Fatal(err)
		}
		ping, ok := msg.(proto.Ping)
		if !ok {
			t.Fatalf("expected ping, got %T", msg)
		}
		pong := proto.Pong{Seq: ping.Seq, SentAt: ping.SentAt}
		if err := proto.Write(control, proto.Pong{Seq: pong.Seq, SentAt: pong.SentAt + 1}); err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(control, pong); err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(control, pong); err != nil {
			t.Fatal(err)
		}
		if err := proto.Write(control, proto.VehicleState{Seq: uint64(i + 1)}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-marker:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case sample := <-samples:
			if sample < 0 {
				t.Fatal("negative RTT")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		select {
		case <-samples:
			t.Fatal("duplicate or invalid Pong recorded")
		default:
		}
	}
}
