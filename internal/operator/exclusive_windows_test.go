//go:build windows

package operator

import (
	"os"
	"testing"

	"remote-enet/internal/exclusive"
)

func TestNativeExclusiveDefaultResolver(t *testing.T) {
	opener, requests, _ := newEchoOpener(t)
	p := startPresenter(t, Options{Exclusive: true, Opener: opener, State: testState(t, "127.0.0.2")})
	diag, ctrl, _ := p.Addr()
	d, c := dialTCP(t, diag), dialTCP(t, ctrl)
	nextRequest(t, requests)
	nextRequest(t, requests)
	snapshot := p.ExclusiveState()
	if !snapshot.Enabled || snapshot.Owner.PID != uint32(os.Getpid()) || snapshot.Owner.Created == 0 || snapshot.Active != 2 {
		t.Fatalf("default resolver did not share owner across ports: %+v", snapshot)
	}
	roundTrip(t, d, []byte("diagnostic"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true, Owner: snapshot.Owner, Active: 1})
	roundTrip(t, c, []byte("control"))
	waitExclusive(t, p, exclusive.Snapshot{Enabled: true})
}
