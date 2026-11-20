package desktop

import (
	"context"
	"log/slog"
)

type runtimeLogHandler struct {
	controller *Controller
	run        *running
}

func (h runtimeLogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelInfo
}

func (h runtimeLogHandler) Handle(_ context.Context, record slog.Record) error {
	switch record.Message {
	case "operator stopped", "operator connection rejected", "operator session ended",
		"client stopped", "client connection ended; retrying":
	default:
		return nil
	}
	if h.run.center {
		h.record(record.Message)
		return nil
	}
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key != "error" {
			return true
		}
		if err, ok := attr.Value.Any().(error); ok {
			h.record(displayRuntimeError(record.Message + ": " + err.Error()))
		}
		return false
	})
	return nil
}

// Only per-record error attributes from the selected app events are exported.
func (h runtimeLogHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h runtimeLogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	return slog.DiscardHandler
}

func (h runtimeLogHandler) record(detail string) {
	c := h.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.run != h.run || h.run.stopping {
		return
	}
	c.eventLocked(detail)
	c.publishLocked()
}
