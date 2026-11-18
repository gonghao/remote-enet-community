package desktopui

import (
	"context"
	"errors"

	"remote-enet/internal/app"
	"remote-enet/internal/desktop"
	"remote-enet/internal/exclusive"
	"remote-enet/internal/linktest"
)

// Request errors never replace the controller's product text with raw I/O,
// protocol, credential or filesystem details. Admission failures may not have
// published a controller error, so those get a small fixed fallback vocabulary.
func operationMessage(err error, productError string) string {
	if err == nil || errors.Is(err, context.Canceled) || productError != "" {
		return ""
	}
	switch {
	case errors.Is(err, desktop.ErrBusy), errors.Is(err, linktest.ErrBusy):
		return "Another operation is still finishing. Please try again."
	case errors.Is(err, context.DeadlineExceeded):
		return "The operation timed out. Please try again."
	case errors.Is(err, desktop.ErrClosed):
		return "The application is shutting down."
	case errors.Is(err, desktop.ErrRole):
		return "Use the configuration for this application."
	case errors.Is(err, exclusive.ErrUnsupported), errors.Is(err, linktest.ErrUnsupported):
		return "This action is not supported on this system or by the peer."
	case errors.Is(err, app.ErrNotConnected):
		return "Connect to a peer before testing."
	default:
		return "The action could not be completed. Check the saved settings and connection status."
	}
}
