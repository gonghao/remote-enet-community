//go:build !darwin

package carside

import (
	"context"
	"syscall"
)

// Other platforms retain their existing source-address selection. IP_BOUND_IF
// is Darwin-specific and requires no global route changes or elevated access.
func bindVehicleInterface(ctx context.Context, _ NIC, _ syscall.RawConn) error { return ctx.Err() }
