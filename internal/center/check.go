package center

import (
	"context"
	"net/http"
	"time"

	"remote-enet/internal/settings"
)

const fallbackAttemptTimeout = 10 * time.Second

// Status describes the outcome of checking one configured entry.
type Status string

const (
	Verified    Status = "verified"
	Unreachable Status = "unreachable"
	IDMismatch  Status = "id_mismatch"
)

// EntryResult pairs the configured URL with the outcome of its metadata check.
type EntryResult struct {
	Entry  string
	Status Status
}

// Check probes every configured entry in order, each with its own deadline.
// Cancelling ctx stops the scan and returns any results obtained so far.
func Check(ctx context.Context, c settings.Center, role, appVersion string) []EntryResult {
	return check(ctx, &http.Client{}, c, role, appVersion, fallbackAttemptTimeout)
}

func check(ctx context.Context, client *http.Client, c settings.Center, role, appVersion string, timeout time.Duration) []EntryResult {
	var results []EntryResult
	for _, entry := range c.API {
		if ctx.Err() != nil {
			break
		}
		attempt, cancel := context.WithTimeout(ctx, timeout)
		meta, err := FetchMeta(attempt, client, entry, role, appVersion)
		cancel()
		if ctx.Err() != nil {
			break
		}
		status := Verified
		switch {
		case err != nil:
			status = Unreachable
		case meta.CenterID != c.ID:
			status = IDMismatch
		}
		results = append(results, EntryResult{Entry: entry, Status: status})
	}
	return results
}
