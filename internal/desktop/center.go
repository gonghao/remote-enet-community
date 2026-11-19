package desktop

import (
	"context"
	"slices"

	"remote-enet/internal/center"
	"remote-enet/internal/settings"
)

// CheckCenter returns the saved center snapshot alongside its results so the
// UI can discard them if the user changes the center during the request.
func (c *Controller) CheckCenter(ctx context.Context) (settings.Center, []center.EntryResult, error) {
	c.mu.Lock()
	ctx, finish, _, err := c.operationLocked(ctx, false)
	role, cfg := c.state.Config.Role, c.state.Config.Center
	cfg.API = slices.Clone(cfg.API)
	c.mu.Unlock()
	if err != nil {
		return settings.Center{}, nil, err
	}
	defer finish()

	results := c.deps.checkCenter(ctx, cfg, role, c.opts.AppVersion)
	return cfg, results, ctx.Err()
}
