//go:build !windows

package exclusive

import (
	"context"
	"errors"
	"testing"
)

func TestDefaultResolverUnsupported(t *testing.T) {
	if owner, err := NewResolver().Resolve(t.Context(), testLocal, testRemote); owner != (Identity{}) || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported resolver: owner=%+v err=%v", owner, err)
	}
	g := NewGate(nil)
	if lease, err := g.Admit(t.Context(), testLocal, testRemote); lease != nil || !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported gate: lease=%v err=%v", lease, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := NewResolver().Resolve(ctx, testLocal, testRemote); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
