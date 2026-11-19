package desktopui

import (
	"context"

	"remote-enet/internal/pairing"
	renetui "remote-enet/internal/ui"
)

func (s *shell) issueNumericCode() {
	var display pairing.Display
	s.submitWithCompletion(renetui.ActionIssueNumericCode, func(ctx context.Context) error {
		var err error
		display, err = s.controller.IssueNumericCode(ctx)
		return err
	}, func() {
		s.view.SetNumericCodeDisplay(display)
	})
}

func (s *shell) copyNumericCode(generation uint64) {
	var clipboard string
	s.submitWithCompletion(renetui.ActionCopyNumericCode, func(ctx context.Context) error {
		var err error
		clipboard, err = s.controller.CopyNumericCode(ctx, generation)
		return err
	}, func() {
		if s.view.NumericCodeCurrent(generation) {
			s.copyText(clipboard)
		}
	})
}
