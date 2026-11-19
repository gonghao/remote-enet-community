// Package ui presents the desktop workspace without owning I/O or runtime lifetimes.
package ui

import (
	"fmt"
	"strconv"
	"strings"

	"remote-enet/internal/app"
	"remote-enet/internal/carside"
	"remote-enet/internal/checkup"
	"remote-enet/internal/hsfzcapture"
	"remote-enet/internal/identity"
	"remote-enet/internal/linktest"
	"remote-enet/internal/pairing"
	"remote-enet/internal/settings"
)

// State contains display data only; invitation codes must stay out of Events.
type State struct {
	Capture                          hsfzcapture.Snapshot
	Runtime                          app.Snapshot
	Running, Stopping, Testing       bool
	DirectWorking                    bool
	Invitation, Notice, Error        string
	NumericCode                      pairing.Status
	NICs                             []carside.NIC
	TestResult                       linktest.Result
	Events                           []string
	Permission                       Permission
	CaptureMarkerShortcut            string
	CaptureMarkerShortcutUnavailable bool
	ChosenLanguage                   string
	Checking                         bool
	Checkup                          checkup.Report
}

// Permission carries capability flags without filesystem paths or credentials.
type Permission struct {
	Platform                                         string
	Elevated, HelperAvailable, AuthorizationRequired bool
}

// Actions run synchronously on the UI thread and must return promptly. The
// integrator queues controller work and handles explicit clipboard requests.
type Actions struct {
	Save              func(settings.Config)
	Refresh           func()
	Connect           func(string)
	Listen            func()
	Stop              func()
	IssueInvite       func()
	IssueNumericCode  func()
	CopyNumericCode   func(uint64)
	RevokeNumericCode func(uint64)
	TestLink          func()
	ConnectMGU        func()
	ConnectEVO        func()
	DisconnectMGU     func()
	CopyDiagnostics   func()
	Copy              func(string)
	StartCapture      func()
	StopCapture       func()
	MarkCapture       func(string) bool
	SetLanguage       func(string)
	CheckConnection   func(string)
	CancelChecks      func()
	Quit              func()
}

// Action identifies an asynchronous request completed by the integrator.
type Action uint8

const (
	ActionSave Action = iota + 1
	ActionRefresh
	ActionConnect
	ActionListen
	ActionStop
	ActionInvite
	ActionTest
	ActionConnectMGU
	ActionDisconnectMGU
	ActionStartCapture
	ActionStopCapture
	ActionCheckConnection
	ActionCancelChecks
	ActionIssueNumericCode
	ActionCopyNumericCode
	ActionRevokeNumericCode
)

// View and all of its methods belong to the UI thread.
type View struct {
	actions                             Actions
	role                                string
	language                            string
	page                                page
	peerDetailsFrom                     page
	detailsPeer                         identity.Fingerprint
	state                               State
	saved                               settings.Config
	draft                               settings.Config
	ports                               [3]string
	invitation                          string
	inputGeneration                     uint64
	numericDisplay                      pairing.Display
	numericAction                       Action
	submitted                           *settings.Config
	savingAdapter                       bool
	refreshing                          bool
	connecting                          bool
	listening                           bool
	stopping                            bool
	testing                             bool
	issuing                             bool
	directStarting                      bool
	directStopping                      bool
	captureStarting, captureStopping    bool
	captureDirection, captureConnection string
	captureSelected                     uint64
	captureHasSelection                 bool
	captureMarkerLabel                  string
	captureAfterMarker                  *hsfzcapture.Marker
	checking, cancelingChecks           bool
}

// New fixes the application role for this view's lifetime and keeps edits
// separate from the persisted configuration.
func New(initial settings.Config, actions Actions) *View {
	v := &View{actions: actions, role: initial.Role, saved: initial}
	v.resetDraft(initial)
	if initial.Role == settings.RoleOperator && (initial.Operator.Endpoint == "" || initial.Operator.Bind == "") {
		v.page = settingsPage
	}
	return v
}

// SetConfig accepts the controller's persisted configuration before SetState.
// Only a matching submission acknowledges Save; unrelated snapshots cannot
// acknowledge it or discard edits made while the save was in flight.
// Configurations for another application role are ignored in their entirety.
func (v *View) SetConfig(cfg settings.Config) {
	if cfg.Role != v.role {
		return
	}
	clean := !v.dirty()
	v.draft.Client.Peer = cfg.Client.Peer
	if v.submitted != nil {
		v.submitted.Client.Peer = cfg.Client.Peer
		if v.submitted.Equal(cfg) {
			current, err := v.config()
			clean = err == nil && current.Equal(cfg)
			v.submitted = nil
			v.savingAdapter = false
		}
	}
	v.saved = cfg
	if clean {
		v.resetDraft(cfg)
	}
}

// SetState never replaces the form. Callers must marshal updates to the UI thread.
func (v *View) SetState(state State) {
	if state.Running {
		v.connecting, v.listening = false, false
	}
	if state.Stopping || !state.Running {
		v.stopping = false
	}
	if state.Testing || state.TestResult != v.state.TestResult || !state.Running {
		v.testing = false
	}
	if state.Invitation != "" && state.Invitation != v.state.Invitation || state.Stopping {
		v.issuing = false
	}
	if state.Running && !v.state.Running {
		v.clearPairingInput()
	}
	if !state.Capture.StartedAt.Equal(v.state.Capture.StartedAt) {
		v.captureHasSelection = false
		v.captureMarkerLabel = ""
		v.captureAfterMarker = nil
	}
	state.Capture.Recent = append([]hsfzcapture.Frame(nil), state.Capture.Recent[max(0, len(state.Capture.Recent)-200):]...)
	state.Capture.RecentMarkers = append([]hsfzcapture.Marker(nil), state.Capture.RecentMarkers[max(0, len(state.Capture.RecentMarkers)-200):]...)
	state.Checkup.Findings = append([]checkup.Finding(nil), state.Checkup.Findings[:min(len(state.Checkup.Findings), 32)]...)
	if v.role == settings.RoleOperator && !v.detailsPeer.IsZero() && state.Runtime.Peer != v.detailsPeer {
		v.detailsPeer = identity.Fingerprint{}
		if v.page == peerDetailsPage {
			v.page = sessionPage
		}
	}
	v.state = state
	v.clearExpiredNumericCode()
}

// Complete releases the action's request latch after SetConfig and SetState.
// The integrator must reject stale completions using its per-action request IDs.
// An unacknowledged home-page adapter save restores only the draft NIC fields
// from the saved baseline. Settings saves preserve the form on failure.
func (v *View) Complete(action Action) {
	switch action {
	case ActionSave:
		if v.submitted != nil && v.savingAdapter {
			v.draft.Client.NICName = v.saved.Client.NICName
			v.draft.Client.NICAddr = v.saved.Client.NICAddr
		}
		v.submitted = nil
		v.savingAdapter = false
	case ActionRefresh:
		v.refreshing = false
	case ActionConnect:
		v.connecting = false
	case ActionListen:
		v.listening = false
	case ActionStop:
		v.stopping = false
	case ActionInvite:
		v.issuing = false
	case ActionTest:
		v.testing = false
	case ActionConnectMGU:
		v.directStarting = false
	case ActionDisconnectMGU:
		v.directStopping = false
	case ActionStartCapture:
		v.captureStarting = false
	case ActionStopCapture:
		v.captureStopping = false
	case ActionCheckConnection:
		v.checking = false
	case ActionCancelChecks:
		v.cancelingChecks = false
	case ActionIssueNumericCode, ActionCopyNumericCode, ActionRevokeNumericCode:
		if v.numericAction == action {
			v.numericAction = 0
		}
	}
}

func (v *View) resetDraft(cfg settings.Config) {
	v.draft = cfg
	v.ports = [3]string{
		strconv.Itoa(cfg.Operator.DiagnosticPort),
		strconv.Itoa(cfg.Operator.ControlPort),
		strconv.Itoa(cfg.Operator.DiscoveryPort),
	}
}

func (v *View) config() (settings.Config, error) {
	cfg := v.draft
	for i, port := range []*int{&cfg.Operator.DiagnosticPort, &cfg.Operator.ControlPort, &cfg.Operator.DiscoveryPort} {
		value, err := strconv.Atoi(v.ports[i])
		if err != nil || value < 1 || value > 65535 {
			return cfg, fmt.Errorf("%s", v.tr("%s port must be between 1 and 65535.", v.tr([]string{"Diagnostic TCP", "Control TCP", "Discovery UDP"}[i])))
		}
		*port = value
	}
	return cfg, cfg.Validate()
}

func (v *View) dirty() bool {
	cfg, err := v.config()
	return err != nil || !cfg.Equal(v.saved)
}

func (v *View) locked() bool {
	return v.state.Running || v.state.Stopping || v.connecting || v.listening || v.stopping
}

func (v *View) ready() bool {
	return !v.locked() && v.submitted == nil && !v.dirty() && v.saved.Validate() == nil
}

func (v *View) canConnect() bool {
	return v.actions.Connect != nil && v.ready() && (strings.TrimSpace(v.invitation) != "" || v.saved.Client.Peer != "")
}

func (v *View) canListen() bool {
	return v.actions.Listen != nil && v.ready() && v.saved.Operator.Listen != "" && v.saved.Operator.Endpoint != "" && v.saved.Operator.Bind != ""
}

func (v *View) canInvite() bool {
	return v.actions.IssueInvite != nil && !v.state.Stopping && !v.stopping && !v.connecting && !v.listening && !v.issuing && v.submitted == nil && !v.dirty() && v.saved.Operator.Endpoint != "" && v.saved.Validate() == nil
}

func (v *View) canStop() bool {
	return v.actions.Stop != nil && v.state.Running && !v.state.Stopping && !v.stopping
}

func (v *View) canTest() bool {
	return v.actions.TestLink != nil && v.state.Running && v.state.Runtime.Phase == app.PhaseConnected && !v.state.Stopping && !v.stopping && !v.state.Testing && !v.testing && v.saved.AllowLinkTest
}
