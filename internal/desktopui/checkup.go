package desktopui

import (
	"context"

	renetui "remote-enet/internal/ui"
)

func (s *shell) checkConnection(invitation string) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.checksCancel = cancel
	if !s.submit(renetui.ActionCheckConnection, func(context.Context) error {
		defer cancel()
		return s.controller.CheckConnection(ctx, invitation)
	}) {
		cancel()
		s.checksCancel = nil
	}
}
