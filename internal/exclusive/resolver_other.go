//go:build !windows

package exclusive

import (
	"context"
	"net/netip"
)

type systemResolver struct{}

func (systemResolver) Resolve(ctx context.Context, _, _ netip.AddrPort) (Identity, error) {
	if err := ctx.Err(); err != nil {
		return Identity{}, err
	}
	return Identity{}, ErrUnsupported
}
