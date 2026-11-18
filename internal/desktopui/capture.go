package desktopui

import (
	"context"
	"errors"
	"time"

	"github.com/egoist/mygo"

	renetui "remote-enet/internal/ui"
)

// The native picker belongs to the UI thread. Its result is buffered so a
// canceled worker never waits for the user to dismiss a late picker.
func (s *shell) startCapture() {
	if s.quitting || s.captureCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	s.captureCancel = cancel
	if !s.submit(renetui.ActionStartCapture, func(context.Context) error {
		defer cancel()
		path, err := s.capturePath(ctx)
		if err != nil || path == "" {
			return err
		}
		return s.controller.StartCapture(ctx, path)
	}) {
		cancel()
		s.captureCancel = nil
	}
}

func (s *shell) capturePath(ctx context.Context) (string, error) {
	type result struct {
		path string
		err  error
	}
	selected := make(chan result, 1)
	s.post(func() {
		if ctx.Err() != nil || s.quitting {
			return
		}
		choose := s.chooseCaptureFile
		if choose == nil {
			choose = func() (string, error) {
				return mygo.Dialog.Save(mygo.SaveDialogOptions{
					Parent: s.window, Title: s.tr("Record HSFZ to a new JSONL file"),
					DefaultPath: "hsfz-" + time.Now().Format("20060102-150405") + ".jsonl",
					ButtonLabel: s.tr("Start capture"), Message: s.tr("Choose a new local file. Existing capture files are never overwritten."),
					Filters: []mygo.FileFilter{{Name: s.tr("HSFZ JSONL capture"), Extensions: []string{"jsonl"}}},
				})
			}
		}
		path, err := choose()
		if err != nil {
			err = errors.New("capture file picker failed")
		}
		selected <- result{path, err}
	})
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case r := <-selected:
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return r.path, r.err
	}
}
