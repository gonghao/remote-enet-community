package tunadapter

import (
	"context"
	"fmt"
	"net/netip"
	"time"
)

const addressReadyTimeout = 5 * time.Second
const addressPollInterval = 50 * time.Millisecond

// Values follow Windows NL_DAD_STATE.
const (
	dadInvalid int32 = iota
	dadTentative
	dadDuplicate
	dadDeprecated
	dadPreferred
)

// Observations own their address memory.
type addressObservation struct {
	luid   uint64
	prefix netip.Prefix
	up     bool
	dad    int32
}
type addressProbe func(context.Context) ([]addressObservation, error)

func addressReady(luid uint64, local netip.Prefix, rows []addressObservation) (bool, error) {
	ready := false
	for _, row := range rows {
		if row.luid != luid || row.prefix.Addr() != local.Addr() {
			continue
		}
		if row.prefix != local {
			return false, fmt.Errorf("local address prefix changed to %s", row.prefix)
		}
		switch row.dad {
		case dadTentative:
			continue // Tentative: another entry may already be preferred.
		case dadPreferred:
			// Check every matching entry: a later fatal state must override readiness.
			ready = ready || row.up
		default:
			return false, fmt.Errorf("local address %s has unusable DAD state %d", local, row.dad)
		}
	}
	return ready, nil
}
func waitAddressReady(ctx context.Context, luid uint64, local netip.Prefix, budget time.Duration, probe addressProbe, pause func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	for {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("wait for local address %s on LUID %d: %w", local, luid, err)
		}
		rows, err := probe(ctx)
		if err != nil {
			return fmt.Errorf("query local address readiness: %w", err)
		}
		if err = ctx.Err(); err != nil {
			return fmt.Errorf("query local address %s: %w", local, err)
		}
		ready, err := addressReady(luid, local, rows)
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if err = pause(ctx); err != nil {
			return fmt.Errorf("wait for local address %s on LUID %d: %w", local, luid, err)
		}
	}
}
func pauseAddressPoll(ctx context.Context) error {
	timer := time.NewTimer(addressPollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
