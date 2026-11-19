package ui

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/app"
	"remote-enet/internal/settings"
)

func (v *View) stage() (string, string, ui.Color) {
	if v.state.Stopping || v.stopping {
		return "Stopping", "Closing the runtime and releasing its resources.", amber
	}
	if v.connecting || v.listening {
		return "Starting", "Starting with the saved configuration.", blue
	}
	phase := v.state.Runtime.Phase
	if !v.state.Running && phase != app.PhaseStopped {
		return "Idle", "Configure your workspace, then start when you are ready.", muted
	}
	switch phase {
	case app.PhaseStarting:
		return "Starting", "Preparing the runtime with saved settings.", blue
	case app.PhaseWaitingNIC:
		return "Waiting for adapter", "The selected vehicle-side IPv4 adapter is unavailable.", amber
	case app.PhaseDiscovering:
		return "Discovering gateway", "Looking for an ENET gateway on the selected adapter.", blue
	case app.PhaseWaitingVehicle:
		return "Waiting for gateway", "No unambiguous gateway is available yet.", amber
	case app.PhaseConnecting:
		return "Connecting", "Establishing the peer session.", blue
	case app.PhaseListening:
		return "Listening", "Waiting for a client to connect.", blue
	case app.PhaseConnected:
		return "Connected", "The peer session is established.", green
	case app.PhaseDisconnected:
		return "Disconnected", "The peer session has ended.", amber
	case app.PhaseBackoff:
		return "Retry scheduled", "Waiting before the next connection attempt.", amber
	case app.PhaseStopped:
		return "Stopped", "The runtime has stopped. Review settings or start again.", muted
	default:
		return "Idle", "Configure your workspace, then start when you are ready.", muted
	}
}

func (v *View) phase(c *ui.Context) {
	title, description, color := v.stage()
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Children(func() {
		ui.Row(c).Gap(8).AlignItems(ui.Center).Shrink(0).Children(func() {
			ui.Box(c).Size(8, 8).Background(color).Shrink(0)
			ui.Text(c, v.tr(title)).FontSize(15).Bold().TextColor(color).Role(ui.RoleStatus).MaxWidth(230).Shrink(0)
		})
		if v.role == settings.RoleOperator {
			if v.state.Runtime.Phase == app.PhaseConnected && v.state.Running {
				description = v.tr("Current client · %s", "—")
				if !v.state.Runtime.Peer.IsZero() {
					description = v.tr("Current client · %s", v.state.Runtime.Peer.String())
				}
			} else {
				description = "No client connected · one session at a time"
			}
		}
		ui.Text(c, v.tr(description)).Grow(1).MinWidth(0).SingleLine().TruncateMiddle().FontSize(11).TextColor(muted).Tooltip(v.tr(description))
	})
}

func (v *View) information(c *ui.Context) {
	w, _ := c.Size()
	if w < 620 && v.role == settings.RoleClient {
		ui.Column(c.Key("client-information")).FillWidth().MinWidth(0).Padding(4, 12).Gap(4).Background(panel).Border(1, line).Radius(0).Children(func() {
			v.informationSections(c, true)
		})
		return
	}
	columns := 3
	if w < 620 {
		columns = 1
	}
	ui.Grid(c).Columns(columns).FillWidth().Gap(12).Children(func() {
		v.informationSections(c, false)
	})
}

func (v *View) informationSections(c *ui.Context, compact bool) {
	section := func(key, title string, rows func()) {
		if !compact {
			v.card(c.Key(key), title, "", rows)
			return
		}
		ui.Column(c.Key(key)).FillWidth().MinWidth(0).Children(func() {
			ui.Text(c, v.tr(title)).FontSize(11).Bold()
			rows()
		})
	}
	value := func(label, text string) {
		row := v.value(c, label, text)
		if compact {
			row.MinHeight(16)
		}
	}
	s := v.state.Runtime
	section("vehicle", "VEHICLE", func() {
		value("VIN", s.VIN)
		value("Gateway IPv4", address(s.Gateway))
		value("Adapter", s.NIC.Name)
		value("Source IPv4", address(s.NIC.Address))
	})
	section("connection", "CONNECTION", func() {
		peer := ""
		if !s.Peer.IsZero() {
			peer = s.Peer.String()
		}
		value("Peer", peer)
		value("Peer address", s.Path.RemoteAddr)
		value("Path", v.tr(string(s.Path.Kind)))
		value("Via", s.Path.Via)
	})
	section("session", "SESSION", func() {
		if v.role == settings.RoleOperator {
			value("Diagnostic", addrPort(s.Diagnostic))
			value("Control", addrPort(s.Control))
			value("Discovery", addrPort(s.Discovery))
			value("Exclusive", v.exclusiveText())
		} else {
			phase, _, _ := v.stage()
			value("State", v.tr(phase))
			generation := ""
			if s.Generation > 0 {
				generation = strconv.FormatUint(s.Generation, 10)
			}
			value("Generation", generation)
			value("Local address", s.Path.LocalAddr)
			retry := ""
			if !s.NextRetry.IsZero() && s.Phase == app.PhaseBackoff && v.state.Running {
				retry = s.NextRetry.Local().Format("15:04:05 MST")
			}
			value("Next retry", retry)
		}
	})
}

func (v *View) exclusiveText() string {
	s := v.state.Runtime
	if s.Exclusive.Enabled {
		if s.Exclusive.Owner.PID != 0 {
			return v.tr("PID %d · %d active", s.Exclusive.Owner.PID, s.Exclusive.Active)
		}
		return v.tr("Enabled · no owner")
	}
	if v.state.Running && s.Phase == app.PhaseConnected {
		return v.tr("Disabled")
	}
	return ""
}

func (v *View) traffic(c *ui.Context) {
	s := v.state.Runtime
	rtt, connections, received, sent, rateIn, rateOut := "—", "—", "—", "—", "—", "—"
	if s.RTTValid {
		rtt = duration(s.RTT)
	}
	if !s.Metrics.At.IsZero() {
		connections = strconv.Itoa(s.Metrics.ActiveConnections)
		received, sent = bytes(s.Metrics.Totals.In), bytes(s.Metrics.Totals.Out)
		if s.Metrics.Rate.Window > 0 {
			rateIn, rateOut = rate(s.Metrics.Rate.InBytesPerSecond), rate(s.Metrics.Rate.OutBytesPerSecond)
		}
	}
	w, _ := c.Size()
	columns := 4
	if w < 840 {
		columns = 2
	}
	compact := v.role == settings.RoleClient && columns == 2
	gap := float32(8)
	if compact {
		gap = 4
		if w < 360 {
			columns = 1
		}
	}
	ui.Grid(c).Columns(columns).FillWidth().Gap(gap).Children(func() {
		v.metric(c, "Peer RTT", rtt, "Latest measurement", compact)
		v.metric(c, "Received total", received, rateIn, compact)
		v.metric(c, "Sent total", sent, rateOut, compact)
		v.metric(c, "Active tunnels", connections, "Local observations", compact)
	})
	if s.MetricsIncomplete {
		ui.Text(c, v.tr("Traffic observations are incomplete; totals may be partial.")).FontSize(11).TextColor(amber)
	}
}

func (v *View) metric(c *ui.Context, label, text, detail string, compact bool) {
	vertical, horizontal, gap := float32(8), float32(12), float32(4)
	if compact {
		vertical, horizontal, gap = 4, 8, 0
	}
	ui.Column(c).FillWidth().MinWidth(0).Padding(vertical, horizontal).Gap(gap).Background(panel).Border(1, line).Radius(0).Children(func() {
		ui.Text(c, v.tr(label)).FontSize(11).TextColor(muted)
		ui.Text(c, text).Font(metricFontFamily).FontSize(24).FontWeight(700).FontFeatures("calt=0", "liga=0").TextColor(blue).SingleLine()
		ui.Text(c, v.tr(detail)).FontSize(10).TextColor(muted).SingleLine()
	})
}

func (v *View) linkTest(c *ui.Context, compact ...bool) {
	v.card(c.Key("test-card"), "Connection test", "Peer echo only · no vehicle test traffic", func() {
		label, hint := "Test connection", "Connect a peer to run a test."
		switch {
		case v.state.Testing || v.testing:
			label, hint = "Testing…", "Echo exchange running · Stop also cancels the test."
		case !v.saved.AllowLinkTest:
			hint = "Connection tests are disabled in saved settings."
		case v.canTest():
			hint = "Both peers must support and allow connection tests."
		}
		if ui.Button(c.Key("test-link"), v.tr(label)).Label(v.tr("Test connection")).FillWidth().Height(32).Disabled(!v.canTest()).Clicked() {
			v.testing = true
			v.actions.TestLink()
		}
		ui.Text(c, v.tr(hint)).FontSize(11).TextColor(muted).Role(ui.RoleStatus)
		result := v.state.TestResult
		if result.Duration == 0 && result.RequestedSamples == 0 && !result.TimedOut && !result.Interrupted {
			ui.Text(c, v.tr("No test result yet.")).FontSize(11).TextColor(muted)
			return
		}
		status := "Last test completed"
		if result.TimedOut {
			status = "Last test timed out · partial results"
		} else if result.Interrupted {
			status = "Last test interrupted · partial results"
		}
		ui.Text(c, v.tr(status)).FontSize(11).Bold()
		p50, p95, echo := "—", "—", "—"
		if result.Samples > 0 {
			p50, p95 = duration(result.RTTP50), duration(result.RTTP95)
		}
		if result.PayloadDuration > 0 && result.EchoedBytes > 0 {
			echo = rate(result.EchoBytesPerSecond)
		}
		w, _ := c.Size()
		columns := 4
		if w < 620 || len(compact) != 0 && compact[0] {
			columns = 2
		}
		ui.Grid(c).Columns(columns).FillWidth().Gap(12).Children(func() {
			v.value(c, "Echo RTT p50", p50)
			v.value(c, "Echo RTT p95", p95)
			v.value(c, "Echo throughput", echo)
			v.value(c, "Validated probes", v.tr("%d / %d", result.Samples, result.RequestedSamples))
		})
		ui.Text(c, v.tr("Echo throughput includes the round trip; it is not upload or download capacity.")).FontSize(10).TextColor(muted)
	})
}

func duration(d time.Duration) string {
	return fmt.Sprintf("%.1f ms", float64(d)/float64(time.Millisecond))
}

func bytes(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	for _, unit := range []string{"KiB", "MiB", "GiB", "TiB", "PiB", "EiB"} {
		value /= 1024
		if value < 1024 || unit == "EiB" {
			return fmt.Sprintf("%.1f %s", value, unit)
		}
	}
	panic("unreachable byte unit")
}

func rate(n float64) string {
	if math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return "—"
	}
	if n < 1024 {
		return fmt.Sprintf("%.0f B/s", n)
	}
	for _, unit := range []string{"KiB/s", "MiB/s", "GiB/s", "TiB/s", "PiB/s", "EiB/s"} {
		n /= 1024
		if n < 1024 || unit == "EiB/s" {
			return fmt.Sprintf("%.1f %s", n, unit)
		}
	}
	panic("unreachable rate unit")
}
