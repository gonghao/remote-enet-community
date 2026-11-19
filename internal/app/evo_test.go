package app

import (
	"context"
	"errors"
	"io"
	"net/netip"
	"testing"

	"remote-enet/internal/exclusive"
	"remote-enet/internal/proto"
	"remote-enet/internal/transport"
)

func TestEVODirectSelectsSSHAndOwnsRoute(t *testing.T) {
	for _, tc := range []struct {
		name   string
		banner string
	}{
		{name: "fallback", banner: "Notice\r\nSSH-2.0-fixture\r\n"},
		{name: "missing SSH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			device := memoryDirectDevice(t)
			probes := make(chan proto.OpenTunnel, 2)
			var targets []netip.Addr
			f := newDirectFixture(t, OperatorConfig{
				DirectFactory: func(_ context.Context, addresses []netip.Addr) (DirectDevice, error) {
					targets = addresses
					return device, nil
				},
			}, func(_ context.Context, h proto.OpenTunnel, stream transport.Stream) {
				defer stream.Close()
				probes <- h
				if h.Target == "169.254.199.99:22" {
					_, _ = io.WriteString(stream, tc.banner)
				} else {
					_, _ = io.WriteString(stream, "SSH-2.0-\r\n")
				}
			})
			gen := f.r.Snapshot().Generation
			if err := f.r.ConnectEVO(t.Context(), gen+1); !errors.Is(err, ErrNotConnected) {
				t.Fatalf("stale generation: %v", err)
			}
			err := f.r.ConnectEVO(t.Context(), gen)
			for _, want := range []string{"169.254.199.119:22", "169.254.199.99:22"} {
				select {
				case got := <-probes:
					if got.Kind != "tcp" || got.Target != want {
						t.Fatalf("unexpected probe: %+v", got)
					}
				default:
					t.Fatalf("missing probe of %s", want)
				}
			}
			s := f.r.Snapshot()
			if tc.banner == "" {
				if err == nil || s.Direct.Phase != DirectFailed || len(targets) != 0 || s.Phase != PhaseConnected {
					t.Fatalf("missing SSH installed a route or broke the session: %+v, %v", s.Direct, err)
				}
				return
			}
			want := netip.MustParseAddr("169.254.199.99")
			if err != nil || len(targets) != 1 || targets[0] != want || s.Direct.Phase != DirectReady || s.Direct.Address != want {
				t.Fatalf("unexpected route: %v, state=%+v, err=%v", targets, s.Direct, err)
			}
			if err := f.r.ConnectMGU(t.Context(), gen); !errors.Is(err, exclusive.ErrBusy) {
				t.Fatalf("MGU was admitted while EVO owned the route: %v", err)
			}
			if err := f.r.DisconnectMGU(t.Context(), gen); err != nil || device.closed.Load() != 1 {
				t.Fatalf("EVO route was not released: %v", err)
			}
		})
	}
}
