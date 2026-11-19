package desktop

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"remote-enet/internal/app"
	"remote-enet/internal/linktest"
	"remote-enet/internal/locales"
)

const (
	maxEvents = 200
)

var (
	invitationText = regexp.MustCompile(`(?i)renet1:[a-z0-9=]*`)
	// Dates such as 2026-11-19 also match; prefer hiding a date to leaking a grouped code.
	numericCodeText = regexp.MustCompile(`[0-9０-９](?:[-\s\p{Z}\x0b\x{0085}]*[0-9０-９]){7}`)
)

func (c *Controller) failure(message string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.failureLocked(message, err)
	}
}

// Operation failures use fixed messages in the banner and history.
func (c *Controller) failureLocked(message string, err error) {
	c.setFailureLocked(message, err, "")
}

func (c *Controller) setFailureLocked(message string, err error, pairingHint string) {
	c.state.Notice = ""
	c.state.Error = message
	c.state.PairingHint = pairingHint
	if errors.Is(err, ErrBusy) || errors.Is(err, linktest.ErrBusy) {
		c.state.Error = "Operation busy."
	} else if errors.Is(err, context.DeadlineExceeded) {
		c.state.Error = "Operation timed out."
	} else if errors.Is(err, context.Canceled) {
		c.state.Error = "Operation canceled."
	}
	c.eventLocked(c.state.Error)
	c.publishLocked()
}

func (c *Controller) successLocked(message string) {
	c.state.Notice, c.state.Error = message, ""
	c.state.PairingHint = ""
	c.eventLocked(message)
	c.publishLocked()
}

func (c *Controller) eventLocked(message string) {
	if len(c.state.Events) == maxEvents {
		copy(c.state.Events, c.state.Events[1:])
		c.state.Events = c.state.Events[:maxEvents-1]
	}
	c.state.Events = append(c.state.Events, message)
}

func (c *Controller) publishLocked() {
	c.refreshCaptureLocked()
	for sub := range c.subs {
		select {
		case <-sub.ch:
		default:
		}
		sub.ch <- cloneState(c.state)
	}
}

func cloneState(s State) State {
	s.NICs = slices.Clone(s.NICs)
	s.Events = slices.Clone(s.Events)
	s.Checkup.Findings = slices.Clone(s.Checkup.Findings)
	s.Capture.Recent = slices.Clone(s.Capture.Recent)
	s.Capture.RecentMarkers = slices.Clone(s.Capture.RecentMarkers)
	s.Runtime = cloneRuntime(s.Runtime)
	return s
}

func cloneRuntime(s app.Snapshot) app.Snapshot {
	s.Metrics.Active = slices.Clone(s.Metrics.Active)
	s.Metrics.History = slices.Clone(s.Metrics.History)
	s.Metrics.Events = slices.Clone(s.Metrics.Events)
	if s.Metrics.LastError != nil {
		event := *s.Metrics.LastError
		s.Metrics.LastError = &event
	}
	return s
}

func displayText(value string) string {
	return trimDisplayText(invitationText.ReplaceAllString(value, "[invitation redacted]"))
}

func displayRuntimeError(value string) string {
	value = invitationText.ReplaceAllString(value, "[invitation redacted]")
	return trimDisplayText(numericCodeText.ReplaceAllString(value, "[numeric code redacted]"))
}

func trimDisplayText(value string) string {
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, value)
	if len(value) > 512 {
		value = value[:512]
		for !utf8.ValidString(value) {
			value = value[:len(value)-1]
		}
	}
	return value
}

// Scrub all diagnostic strings, including those supplied by the remote peer.
// Snapshot strings stay separate from the runtime logger’s diagnostic events.
func displayRuntime(s app.Snapshot) app.Snapshot {
	s = cloneRuntime(s)
	if message := locales.PairingMessage(s.LastError); message != "" {
		s.LastError = message
	}
	s.LastError, s.VIN, s.NIC.Name = displayText(s.LastError), displayText(s.VIN), displayText(s.NIC.Name)
	s.Direct.Adapter, s.Direct.LastError = displayText(s.Direct.Adapter), displayText(s.Direct.LastError)
	s.Path.LocalAddr, s.Path.RemoteAddr, s.Path.Via = displayText(s.Path.LocalAddr), displayText(s.Path.RemoteAddr), displayText(s.Path.Via)
	for i := range s.Metrics.Active {
		s.Metrics.Active[i].Target = displayText(s.Metrics.Active[i].Target)
		s.Metrics.Active[i].Error = displayText(s.Metrics.Active[i].Error)
	}
	for i := range s.Metrics.History {
		s.Metrics.History[i].Target = displayText(s.Metrics.History[i].Target)
		s.Metrics.History[i].Error = displayText(s.Metrics.History[i].Error)
	}
	for i := range s.Metrics.Events {
		event := &s.Metrics.Events[i]
		event.Kind, event.Message, event.Error = displayText(event.Kind), displayText(event.Message), displayText(event.Error)
	}
	if event := s.Metrics.LastError; event != nil {
		event.Kind, event.Message, event.Error = displayText(event.Kind), displayText(event.Message), displayText(event.Error)
	}
	return s
}

func phaseEvent(phase app.Phase) string {
	switch phase {
	case app.PhaseStarting:
		return "Runtime starting."
	case app.PhaseWaitingNIC:
		return "Waiting for vehicle interface."
	case app.PhaseDiscovering:
		return "Discovering vehicle."
	case app.PhaseWaitingVehicle:
		return "Waiting for vehicle."
	case app.PhaseConnecting:
		return "Connecting to peer."
	case app.PhaseListening:
		return "Listening for peer."
	case app.PhaseConnected:
		return "Peer connected."
	case app.PhaseDisconnected:
		return "Peer disconnected."
	case app.PhaseBackoff:
		return "Waiting to retry."
	default:
		return ""
	}
}
