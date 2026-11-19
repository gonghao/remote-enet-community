package desktopui

import (
	"context"
	"runtime"
	"sync"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"remote-enet/internal/brand"
	"remote-enet/internal/desktop"
	"remote-enet/internal/settings"
	renetui "remote-enet/internal/ui"
)

type shell struct {
	product                    product
	version                    string
	language                   string
	chosenLanguage             string
	permission                 renetui.Permission
	copyText                   func(string)
	ctx                        context.Context
	cancel                     context.CancelFunc
	controller                 *desktop.Controller
	workers                    sync.WaitGroup
	requests                   chan struct{}
	window                     *mygo.Window
	tray                       *mygo.Tray
	view                       *renetui.View
	state                      desktop.State
	commandErr                 string
	commandNotice              string
	systemNotice               string
	nextID                     uint64
	requestIDs                 map[renetui.Action]uint64
	stopPending                bool
	directStopPending          bool
	captureStopPending         bool
	checksCancelPending        bool
	checksCancel               context.CancelFunc
	captureCancel              context.CancelFunc
	chooseCaptureFile          func() (string, error)
	post                       func(func())
	invalidate                 func()
	quitting                   bool
	quitReady                  bool
	registerCaptureShortcut    func(string, func(string), func()) error
	unregisterCaptureShortcut  func(string)
	captureShortcutRegistered  bool
	captureShortcutAttempt     time.Time
	captureShortcutKeys        string
	captureShortcutUnavailable bool
}

func newShell(ctx context.Context, cancel context.CancelFunc, controller *desktop.Controller, product product) *shell {
	return &shell{product: product, version: "dev", permission: renetui.Permission{Platform: runtime.GOOS}, ctx: ctx, cancel: cancel, controller: controller, requests: make(chan struct{}, 4), requestIDs: make(map[renetui.Action]uint64)}
}

func (s *shell) ready() {
	s.language = mygo.App.Locale()
	s.chosenLanguage = mygo.App.ChosenLocale()
	s.state = s.controller.Snapshot()
	s.copyText = mygo.Clipboard.WriteText
	actions := renetui.Actions{
		Save: func(cfg settings.Config) {
			s.submit(renetui.ActionSave, func(ctx context.Context) error { return s.controller.Save(ctx, cfg) })
		},
		Refresh:         func() { s.submit(renetui.ActionRefresh, s.controller.RefreshNICs) },
		Stop:            func() { s.submit(renetui.ActionStop, s.controller.Stop) },
		TestLink:        func() { s.submit(renetui.ActionTest, s.controller.TestLink) },
		Copy:            mygo.Clipboard.WriteText,
		CopyDiagnostics: s.copyDiagnostics,
		Quit:            mygo.App.Quit,
		SetLanguage:     s.setLanguage,
		CheckConnection: s.checkConnection,
		CancelChecks:    func() { s.submit(renetui.ActionCancelChecks, s.controller.CancelChecks) },
	}
	if s.product.role == settings.RoleClient {
		actions.Connect = func(invitation string) {
			s.submit(renetui.ActionConnect, func(ctx context.Context) error { return s.controller.StartClient(ctx, invitation) })
		}
	} else {
		actions.Listen = func() { s.submit(renetui.ActionListen, s.controller.StartOperator) }
		actions.ConnectMGU = func() { s.submit(renetui.ActionConnectMGU, s.controller.ConnectMGU) }
		actions.ConnectEVO = func() { s.submit(renetui.ActionConnectMGU, s.controller.ConnectEVO) }
		actions.DisconnectMGU = func() { s.submit(renetui.ActionDisconnectMGU, s.controller.DisconnectMGU) }
		actions.StartCapture = s.startCapture
		actions.StopCapture = func() { s.submit(renetui.ActionStopCapture, s.controller.StopCapture) }
		actions.MarkCapture = s.markCapture
		actions.IssueInvite = func() {
			s.submit(renetui.ActionInvite, func(ctx context.Context) error { _, err := s.controller.IssueInvite(ctx); return err })
		}
		actions.IssueNumericCode = s.issueNumericCode
		actions.CopyNumericCode = s.copyNumericCode
		actions.RevokeNumericCode = func(generation uint64) {
			s.submit(renetui.ActionRevokeNumericCode, func(ctx context.Context) error {
				return s.controller.RevokeNumericCode(ctx, generation)
			})
		}
	}
	s.view = renetui.New(s.state.Config, actions)

	workArea := mygo.Screen.PrimaryDisplay().WorkArea
	width, height := s.product.initialSize(workArea.Width, workArea.Height)
	s.window = mygo.NewWindow(mygo.WindowOptions{
		Title: s.tr(s.product.name), Width: width, Height: height,
		MinWidth: s.product.minWidth, MinHeight: s.product.minHeight,
		DisableResize: s.product.fixedSize, DisableMaximize: s.product.fixedSize,
		Frameless: true, Hidden: mainWindowStartsHidden,
		BackgroundColor: renetui.BackgroundColor,
		Content:         ui.View(s.view.Render),
	})
	prepareMainWindow(s.window)
	s.post, s.invalidate = s.window.Update, s.window.Invalidate
	_ = s.window.SetIcon(brand.PNG(256, true, false))
	s.registerCaptureShortcut = func(acc string, keys func(string), trigger func()) error {
		return mygo.GlobalShortcut.RegisterWith(acc, mygo.GlobalShortcutOptions{Description: s.tr("Insert HSFZ capture marker"), OnKeys: keys}, trigger)
	}
	s.unregisterCaptureShortcut = mygo.GlobalShortcut.Unregister
	s.applyState()
	if runtime.GOOS != "darwin" {
		// macOS has the Dock; a menu bar item would only duplicate it.
		s.installTray()
	}
	offLocale := mygo.App.OnLocaleChanged(s.localeChanged)
	s.window.OnClose(func(e *mygo.CloseEvent) {
		// Keep the window alive until the quit path has joined all workers.
		if !s.quitReady {
			e.PreventDefault()
			mygo.App.Quit()
		}
	})
	mygo.App.OnBeforeQuit(func(e *mygo.QuitEvent) {
		if s.quitReady {
			return
		}
		s.view.ClearPairingSecrets()
		s.invalidate()
		if e.SessionEnding {
			s.quitting = true
			s.releaseCaptureShortcut()
			s.cancel()
			_ = s.controller.Close(context.Background())
			s.workers.Wait()
			s.quitReady = true
			return
		}
		e.PreventDefault()
		if s.quitting {
			return
		}
		s.quitting = true
		s.releaseCaptureShortcut()
		s.cancel()
		go func() {
			_ = s.controller.Close(context.Background())
			s.workers.Wait()
			mygo.RunOnMain(func() {
				s.quitReady = true
				mygo.App.Quit()
			})
		}()
	})
	mygo.App.OnWillQuit(func(*mygo.QuitEvent) {
		offLocale()
		s.releaseCaptureShortcut()
		if s.tray != nil {
			s.tray.Destroy()
		}
	})
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		for range s.controller.Subscribe(s.ctx) {
			s.post(func() {
				if !s.quitting {
					s.state = s.controller.Snapshot()
					s.commandErr = ""
					s.applyState()
				}
			})
		}
	}()
	s.submit(renetui.ActionRefresh, s.controller.RefreshNICs)
}

// submit is only called on the UI thread. A small admission budget keeps
// repeated clicks bounded. Stop has one reserved, coalesced request so it
// can cancel pending work even when all ordinary action slots are occupied.
func (s *shell) submit(action renetui.Action, fn func(context.Context) error) bool {
	return s.submitWithCompletion(action, fn, nil)
}

func (s *shell) submitWithCompletion(action renetui.Action, fn func(context.Context) error, complete func()) bool {
	if s.quitting {
		return false
	}
	if (action == renetui.ActionStop || action == renetui.ActionCancelChecks) && s.checksCancel != nil {
		s.checksCancel()
	}
	if (action == renetui.ActionStop || action == renetui.ActionStopCapture) && s.captureCancel != nil {
		s.captureCancel()
	}
	if action == renetui.ActionStop && s.view != nil {
		s.view.ClearPairingSecrets()
		s.invalidate()
	}
	if action == renetui.ActionStop || action == renetui.ActionStopCapture {
		s.releaseCaptureShortcut()
	}
	ordinary := action != renetui.ActionStop && action != renetui.ActionDisconnectMGU && action != renetui.ActionStopCapture && action != renetui.ActionCancelChecks
	if !ordinary {
		pending := &s.stopPending
		if action == renetui.ActionDisconnectMGU {
			pending = &s.directStopPending
		} else if action == renetui.ActionStopCapture {
			pending = &s.captureStopPending
		} else if action == renetui.ActionCancelChecks {
			pending = &s.checksCancelPending
		}
		if *pending {
			return false
		}
		*pending = true
	} else {
		select {
		case s.requests <- struct{}{}:
		default:
			s.commandErr = "Another action is still finishing. Please try again."
			s.view.Complete(action)
			s.applyState()
			s.invalidate()
			return false
		}
	}
	s.commandErr = ""
	s.commandNotice = ""
	s.nextID++
	requestID := s.nextID
	s.requestIDs[action] = requestID
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		if ordinary {
			defer func() { <-s.requests }()
		}
		err := fn(s.ctx)
		s.post(func() {
			if s.quitting {
				return
			}
			if !ordinary {
				if action == renetui.ActionDisconnectMGU {
					s.directStopPending = false
				} else if action == renetui.ActionStopCapture {
					s.captureStopPending = false
				} else if action == renetui.ActionCancelChecks {
					s.checksCancelPending = false
				} else {
					s.stopPending = false
				}
			}
			// Sample on the UI thread: a previously queued subscription must
			// not roll the display back after an operation's completion.
			s.state = s.controller.Snapshot()
			if requestID == s.requestIDs[action] {
				if action == renetui.ActionCheckConnection {
					s.checksCancel = nil
				}
				if action == renetui.ActionStartCapture {
					s.captureCancel = nil
				}
				s.commandErr = operationMessage(err, s.state.Error)
				s.applyState()
				s.view.Complete(action)
				if err == nil && complete != nil && !s.stopPending {
					complete()
				}
			} else {
				s.applyState()
			}
		})
	}()
	return true
}

func (s *shell) applyState() {
	s.syncCaptureShortcut()
	errText, notice := s.displayMessage(s.state.Error), s.displayMessage(s.state.Notice)
	runtime := s.state.Runtime
	if s.state.PairingHint != "" {
		errText += " " + s.tr(s.state.PairingHint)
		if runtime.LastError == s.state.Error {
			runtime.LastError = ""
		}
	}
	systemNotice := s.tr(s.systemNotice)
	if s.commandNotice != "" {
		notice = s.displayMessage(s.commandNotice)
	}
	if s.systemNotice != "" {
		if notice != "" {
			notice += " "
		}
		notice += systemNotice
	}
	if s.commandErr != "" {
		errText, notice = s.displayMessage(s.commandErr), systemNotice
	}
	s.view.SetLanguage(s.language)
	s.view.SetConfig(s.state.Config)
	s.view.SetState(renetui.State{
		Runtime: runtime, Running: s.state.Running,
		Stopping: s.state.Stopping, Testing: s.state.Testing,
		DirectWorking: s.state.DirectWorking,
		Capture:       s.state.Capture,
		Invitation:    s.state.Invitation, Notice: notice, Error: errText,
		NumericCode: s.state.NumericCode,
		NICs:        s.state.NICs, TestResult: s.state.TestResult, Events: s.state.Events,
		Permission:                       s.permission,
		CaptureMarkerShortcut:            s.captureShortcutKeys,
		CaptureMarkerShortcutUnavailable: s.captureShortcutUnavailable,
		ChosenLanguage:                   s.chosenLanguage,
		Checking:                         s.state.Checking,
		Checkup:                          s.state.Checkup,
	})
	if s.tray != nil {
		s.tray.SetToolTip(s.tr(s.product.name) + " — " + s.tr(string(s.state.Runtime.Phase)))
	}
}

func (s *shell) show() {
	if s.window == nil || s.quitting {
		return
	}
	s.window.Show()
	s.window.Focus()
}

func (s *shell) trayMenu() *mygo.Menu {
	return mygo.NewMenu([]*mygo.MenuItem{
		{Label: s.tr("Open %s", s.tr(s.product.name)), Click: func(*mygo.MenuItem, *mygo.Window) { s.show() }},
		{Label: s.tr("Stop relay"), Click: func(*mygo.MenuItem, *mygo.Window) { s.submit(renetui.ActionStop, s.controller.Stop) }},
		mygo.Separator(),
		{Label: s.tr("Quit %s", s.tr(s.product.name)), Click: func(*mygo.MenuItem, *mygo.Window) { mygo.App.Quit() }},
	})
}

func (s *shell) installTray() {
	tray, err := mygo.NewTray(mygo.TrayOptions{Icon: brand.PNG(16, false, false), ToolTip: s.tr(s.product.name), Menu: s.trayMenu()})
	if err != nil {
		s.systemNotice = "Tray is unavailable. Closing the window will quit the application."
		s.applyState()
		return
	}
	s.tray = tray
	tray.OnClick(s.show)
}
