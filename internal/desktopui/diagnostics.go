package desktopui

import "remote-enet/internal/diagnostics"

func (s *shell) copyDiagnostics() {
	if s.quitting {
		return
	}
	latest := s.controller.Snapshot()
	p := s.permission
	data, err := diagnostics.Build(diagnostics.Input{
		Version: s.version, Role: s.product.role, Runtime: latest.Runtime,
		Running: latest.Running, Stopping: latest.Stopping, Testing: latest.Testing, DirectWorking: latest.DirectWorking,
		Permission: diagnostics.Permission{Platform: p.Platform, Elevated: p.Elevated, HelperAvailable: p.HelperAvailable, AuthorizationRequired: p.AuthorizationRequired},
	})
	s.state = latest
	if err != nil {
		s.commandErr = "Diagnostic report could not be copied."
		s.commandNotice = ""
	} else {
		s.copyText(string(data))
		s.commandErr = ""
		s.commandNotice = "Diagnostic report copied."
		s.state.Notice = "Diagnostic report copied."
	}
	s.applyState()
	s.invalidate()
}
