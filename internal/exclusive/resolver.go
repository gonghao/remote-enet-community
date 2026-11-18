package exclusive

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func resolveOwner(ctx context.Context, lookup func(context.Context) (Identity, error)) (Identity, error) {
	ctx, cancel := context.WithTimeout(ctx, resolveTimeout)
	defer cancel()
	delay := 10 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return Identity{}, fmt.Errorf("exclusive: resolve TCP owner: %w: %w", ErrUnknownOwner, err)
		}
		owner, err := lookup(ctx)
		if err == nil {
			if err := ctx.Err(); err != nil {
				return Identity{}, err
			}
			return owner, nil
		}
		if !errors.Is(err, ErrUnknownOwner) {
			return Identity{}, err
		}
		// New endpoints may not be visible in the first table snapshot. Back off
		// between fresh reads instead of rebuilding a large table every 10ms.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Identity{}, fmt.Errorf("exclusive: resolve TCP owner: %w: %w", ErrUnknownOwner, ctx.Err())
		case <-timer.C:
		}
		delay = min(delay*2, 100*time.Millisecond)
	}
}
