package desktop

import (
	"context"
	"errors"
	"net/netip"
	"testing"

	"remote-enet/internal/app"
	"remote-enet/internal/identity"
)

func TestNetworkFactoryIsDeferredUntilExplicitDirectSetup(t *testing.T) {
	called := 0
	denied := errors.New("authorization declined")
	target := netip.MustParseAddr("169.254.91.3")
	factory := func(ctx context.Context, targets []netip.Addr) (app.DirectDevice, error) {
		called++
		if ctx != t.Context() || len(targets) != 1 || targets[0] != target {
			t.Fatal("direct setup lost its request context or target")
		}
		return nil, denied
	}
	c := &Controller{opts: Options{StateDir: t.TempDir(), DirectFactory: factory}}
	_, operator, _, err := c.runtimeConfigs(operatorSettings(), "")
	if err != nil || called != 0 || operator.DirectFactory == nil {
		t.Fatalf("configuration triggered authorization or lost factory: %d %v", called, err)
	}
	device, err := operator.DirectFactory(t.Context(), []netip.Addr{target})
	if device != nil || !errors.Is(err, denied) || called != 1 {
		t.Fatalf("direct setup did not use the injected factory: %v %v", device, err)
	}
	_, _, _, err = c.runtimeConfigs(clientSettings(), "")
	if err != nil || called != 1 {
		t.Fatalf("Client configuration triggered network authorization: %d %v", called, err)
	}
}

func TestHubRuntimeConfig(t *testing.T) {
	c := &Controller{opts: Options{StateDir: t.TempDir()}}
	cfg := operatorSettings()
	_, op, _, err := c.runtimeConfigs(cfg, "")
	if err != nil || op.Hub != nil || op.Listen != cfg.Operator.Listen {
		t.Fatal("direct configuration", err)
	}
	fp := identity.Fingerprint{1}
	cfg.Operator.Mode = "hub"
	cfg.Operator.Hub.Addr, cfg.Operator.Hub.FP = "localhost:47001", fp.String()
	for _, share := range []string{"", "public.example:47002"} {
		cfg.Operator.Hub.Share = share
		_, op, _, err := c.runtimeConfigs(cfg, "")
		if share == "" {
			share = cfg.Operator.Hub.Addr
		}
		if err != nil || op.Hub == nil || op.Hub.FP != fp || op.Hub.Addr != cfg.Operator.Hub.Addr || op.Listen != "" || op.ShareAddress != share {
			t.Fatal("hub configuration", err)
		}
	}
}
