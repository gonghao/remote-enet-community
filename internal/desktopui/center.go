package desktopui

import (
	"context"

	"github.com/egoist/mygo"
	"remote-enet/internal/center"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/settings"
	renetui "remote-enet/internal/ui"
)

func (s *shell) centerActions(a *renetui.Actions) {
	c := s.controller
	a.EndReception = func() { s.submit(renetui.ActionEndReception, c.EndReception) }
	a.CenterDisplay = c.CenterDisplay
	a.AnswerSAS = func(id uint64, match bool) {
		s.submit(renetui.ActionAnswerSAS, func(ctx context.Context) error { return c.AnswerSAS(ctx, id, match) })
	}
	if s.product.role == settings.RoleClient {
		a.CenterSessionCandidates = func() []centerflow.SessionCandidate {
			if choice := c.CenterSessionChoice(); choice != nil {
				return choice.Candidates()
			}
			return nil
		}
		a.ResumeCenterSession = func(id center.SessionID) {
			s.submit(renetui.ActionConnect, func(ctx context.Context) error { return c.ResumeCenterSession(ctx, id) })
		}
		a.RetryPairing = func() { s.submit(renetui.ActionRetryPairing, c.RetryPairing) }
		return
	}
	a.MatchSAS = c.MatchSAS
	a.StartReception = func(node string) {
		s.submit(renetui.ActionListen, func(ctx context.Context) error { return c.StartReception(ctx, node) })
	}

	a.RefreshCenter = func() { s.submit(renetui.ActionRefreshCenter, c.RefreshCenter) }
	a.CenterLogin = func(code, label string) {
		s.submit(renetui.ActionAccount, func(ctx context.Context) error { return c.CenterLogin(ctx, code, label) })
	}
	confirm := func(label, detail string) bool {
		result, err := mygo.Dialog.Message(mygo.MessageOptions{Parent: s.window, Title: s.tr(s.product.name), Type: mygo.MessageQuestion, Message: s.tr(label), Detail: detail, Buttons: []string{s.tr("Cancel"), s.tr("Confirm")}, CancelButton: 0, DefaultButton: 0})
		return err == nil && result.Button == 1
	}
	a.CenterLogout = func() {
		if confirm("Sign out", s.tr("Revoke this device and sign out?")) {
			s.submit(renetui.ActionAccount, c.CenterLogout)
		}
	}
	a.RevokeCenterDevice = func(fp string) {
		if confirm("Revoke", fp) {
			s.submit(renetui.ActionAccount, func(ctx context.Context) error { return c.RevokeCenterDevice(ctx, fp) })
		}
	}
}
