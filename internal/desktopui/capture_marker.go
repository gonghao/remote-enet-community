package desktopui

import (
	"errors"
	"fmt"
	"runtime"
	"time"

	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/settings"
)

const captureMarkerAccelerator = "CmdOrCtrl+Shift+M"

func (s *shell) markCapture(label string) bool {
	if s.quitting || s.product.role != settings.RoleOperator {
		return false
	}
	marker, err := s.controller.MarkCapture(s.ctx, label)
	s.state = s.controller.Snapshot()
	s.commandNotice = ""
	if err != nil {
		switch {
		case errors.Is(err, hsfzcapture.ErrNotRecording):
			s.commandErr = "Start HSFZ capture before inserting a marker."
		case errors.Is(err, hsfzcapture.ErrMarkerLabel):
			s.commandErr = "Marker labels must use one line and at most 256 UTF-8 bytes."
		case errors.Is(err, hsfzcapture.ErrMarkerFull):
			s.commandErr = "Marker could not be queued. Capture is busy; try again."
		default:
			s.commandErr = operationMessage(err, "")
		}
	} else {
		s.commandErr = ""
		s.commandNotice = fmt.Sprintf("Marker %d queued.", marker.Sequence)
	}
	s.applyState()
	s.invalidate()
	return err == nil
}

func (s *shell) syncCaptureShortcut() {
	active := s.product.role == settings.RoleOperator && !s.quitting && s.state.Running && !s.state.Stopping && !s.stopPending && !s.captureStopPending && s.state.Capture.Active && !s.state.Capture.Stopping
	if !active {
		s.releaseCaptureShortcut()
		s.captureShortcutAttempt = time.Time{}
		s.captureShortcutUnavailable = false
		return
	}
	epoch := s.state.Capture.StartedAt
	if s.registerCaptureShortcut == nil || s.captureShortcutAttempt.Equal(epoch) {
		return
	}
	s.releaseCaptureShortcut()
	s.captureShortcutUnavailable = false
	s.captureShortcutAttempt = epoch
	s.captureShortcutKeys = "Ctrl+Shift+M"
	if runtime.GOOS == "darwin" {
		s.captureShortcutKeys = "Cmd+Shift+M"
	}
	err := s.registerCaptureShortcut(captureMarkerAccelerator, func(keys string) {
		if !s.quitting && s.captureShortcutAttempt.Equal(epoch) {
			s.captureShortcutKeys = keys
			s.captureShortcutUnavailable = keys == ""
			s.applyState()
			s.invalidate()
		}
	}, func() {
		if s.quitting || !s.captureShortcutRegistered {
			return
		}
		latest := s.controller.Snapshot()
		if !latest.Capture.Active || latest.Capture.Stopping || !latest.Capture.StartedAt.Equal(epoch) {
			return
		}
		s.state = latest
		s.applyState()
		s.view.InsertCaptureMarker()
	})
	if err != nil {
		s.captureShortcutKeys = ""
		s.captureShortcutUnavailable = true
		return
	}
	s.captureShortcutRegistered = true
}

func (s *shell) releaseCaptureShortcut() {
	if s.captureShortcutRegistered {
		s.unregisterCaptureShortcut(captureMarkerAccelerator)
		s.captureShortcutRegistered = false
	}
	s.captureShortcutKeys = ""
}
