package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"remote-enet/internal/centercode"
	"remote-enet/internal/centerflow"
	"remote-enet/internal/locales"
	"remote-enet/internal/settings"
)

func (v *View) refreshCenter() {
	v.node = ""
	v.accountPending = true
	v.actions.RefreshCenter()
}

func (v *View) accountControls(c *ui.Context) {
	s := v.state.Center
	disabled := v.locked() || v.saved.Operator.Mode != "center" || v.accountPending
	if !s.LoggedIn {
		if v.page == settingsPage {
			ui.Text(c, v.tr("Signed out")).FontSize(11)
		}
		ui.Grid(c).FillWidth().Columns(2).Gap(8).Children(func() {
			v.field(c, "Account code", "XXXX-XXXX", &v.accountCode, disabled)
			v.field(c, "Device name (optional)", "", &v.deviceName, disabled)
		})
		code, err := centercode.NormalizeAccountCode(v.accountCode)
		if err == nil {
			v.accountCode = centercode.FormatAccountCode(code)
		}
		ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
			if ui.Button(c.Key("login"), v.tr("Sign in")).Disabled(disabled || err != nil || v.actions.CenterLogin == nil).Clicked() {
				v.accountPending = true
				v.actions.CenterLogin(code, v.deviceName)
			}
			if err != nil && v.accountCode != "" {
				label := "Account code must be 8 characters"
				if err == centercode.ErrCodeCharacter {
					label = "Account code has an invalid character"
				}
				ui.Text(c, v.tr(label)).FontSize(11).TextColor(c.Theme().Danger)
			}
		})
		return
	}
	ui.Text(c, v.tr("Signed in")+" · "+s.Account.Label).FontSize(12)
	deadline := time.Unix(s.EffectiveExpiry, 0)
	color := muted
	if time.Until(deadline) < 7*24*time.Hour {
		color = amber
	}
	ui.Text(c, v.tr("Authorization ends at %s", deadline.Local().Format("2006-01-02 15:04"))+" · "+v.tr("Quota: %d/%d", s.Account.QuotaInUse, s.Account.Quota)).FontSize(11).TextColor(color)
	ui.Row(c).Gap(8).Children(func() {
		if ui.Button(c.Key("devices"), v.tr("Devices")).Clicked() {
			v.showDevices = !v.showDevices
		}
		if ui.Button(c.Key("logout"), v.tr("Sign out")).Disabled(disabled || v.actions.CenterLogout == nil).Clicked() {
			v.actions.CenterLogout()
		}
	})
	if v.showDevices {
		for _, d := range s.Devices {
			fp := d.DeviceFP.String()
			ui.Row(c.Key(fp)).FillWidth().Gap(8).Children(func() {
				label := d.Label + " · " + d.DeviceFP.Short()
				if d.Current {
					label += " · " + v.tr("This device")
				}
				ui.Text(c, label).Grow(1).MinWidth(0).SingleLine().TruncateMiddle().Tooltip(fp)
				if ui.Button(c, v.tr("Revoke")).Disabled(disabled || v.actions.RevokeCenterDevice == nil).Clicked() {
					v.actions.RevokeCenterDevice(fp)
				}
			})
		}
	}
}

func preferredNode(nodes []centerflow.NodeLatency, selected string) string {
	var best *centerflow.NodeLatency
	for i := range nodes {
		n := &nodes[i]
		if n.State != "available" || n.Latency < 0 {
			continue
		}
		if n.NodeID.String() == selected {
			return selected
		}
		if best == nil || n.Latency < best.Latency {
			best = n
		}
	}
	if best != nil {
		return best.NodeID.String()
	}
	return ""
}

func (v *View) nodeControls(c *ui.Context) {
	nodes := v.state.Center.Nodes
	v.node = preferredNode(nodes, v.node)
	sel := ui.SelectBase(c.Key("center-node"), &v.node)
	label := v.tr("Node")
	columns := func(n centerflow.NodeLatency) []string {
		latency := "—"
		if n.Latency >= 0 {
			latency = fmt.Sprintf("%d ms", n.Latency.Milliseconds())
		}
		state := "Unavailable"
		if n.State == "available" {
			state = "Available"
		} else if n.State == "full" {
			state = "Full"
		}
		return []string{n.Region, fmt.Sprintf("%d%%", n.Load), latency, v.tr(state)}
	}
	for _, n := range nodes {
		if n.NodeID.String() == v.node {
			label = strings.Join(columns(n), " · ")
			break
		}
	}
	sel.Trigger.Label(v.tr("Node")).Grow(1).MinWidth(0).Height(32).Padding(4, 8).Border(1, controlStroke).Background(inputSurface).Disabled(v.locked() || len(nodes) == 0 || v.accountPending).Children(func() { ui.Text(c, label).Grow(1).SingleLine().TruncateMiddle(); selectChevron(c) })
	sel.Popup(func(p ui.Element) {
		p.Padding(4).Radius(0).Background(panel).Border(1, line)
		row := func(values []string) {
			ui.Grid(c).Columns(4).FillWidth().Gap(4).Children(func() {
				for _, value := range values {
					ui.Text(c, value).FontSize(11).SingleLine().TruncateMiddle().Tooltip(value)
				}
			})
		}
		row([]string{v.tr("Region"), v.tr("Load"), v.tr("Latency"), v.tr("State")})
		ui.Scroll(c).FillWidth().MaxHeight(200).Children(func() {
			for _, n := range nodes {
				item := sel.Item(n.NodeID.String()).FillWidth().Padding(4, 8).Disabled(n.State != "available" || n.Latency < 0)
				if item.Highlighted() {
					item.Background(c.Theme().Accent).TextColor(c.Theme().AccentText)
				}
				item.Children(func() { row(columns(n)) })
			}
		})
	})
}

func (v *View) centerStage() string {
	switch v.state.Center.Phase {
	case "redeeming":
		return "Redeeming code"
	case "open":
		return "Waiting for redemption"
	case "authorized", "redeemed", "waiting":
		if v.role == settings.RoleClient {
			return "Waiting for the operator"
		}
		return "Waiting for visitor"
	case "confirming":
		return "Verify the short code"
	case "awaiting_center":
		return "Waiting for center confirmation"
	case "active":
		return "Connected"
	case "paused":
		return "Disconnected"
	case "ended":
		return "Session ended"
	case "retry":
		return "Retry pairing"
	}
	if v.role == settings.RoleClient {
		return "Idle"
	}
	if v.state.Center.LoggedIn {
		return "Signed in"
	}
	return "Signed out"
}

func (v *View) centerReason() string {
	reason := v.tr(locales.CenterMessage(string(v.state.Center.Reason), v.role))
	if v.state.Center.Phase == "retry" {
		return strings.TrimSpace(reason + " " + v.tr("Attempts left: %d", v.state.Center.AttemptsLeft))
	}
	return reason
}

func (v *View) centerReception(c *ui.Context) {
	s := v.state.Center
	status := v.tr(v.centerStage())
	if s.LoggedIn && v.centerStage() == "Signed in" {
		status += " · " + s.Account.Label
	}
	ui.Text(c, v.tr("Session controls")+" · "+v.tr("One client at a time")+" · "+v.tr("Center")+" · "+v.saved.Center.ID+" · "+status).Bold().FontSize(12)
	if reason := v.centerReason(); reason != "" {
		ui.Text(c, reason).TextColor(amber)
	}
	if !s.ExpiresAt.IsZero() && v.state.Running && time.Until(s.ExpiresAt) < 10*time.Minute {
		ui.Text(c, v.tr("Authorization ends at %s", s.ExpiresAt.Local().Format("15:04"))).TextColor(amber)
	}
	ui.Grid(c).Columns(pageColumns(c)).FillWidth().Gap(8).Children(func() {
		ui.Column(c).FillWidth().Gap(4).Children(func() { v.accountControls(c) })
		ui.Column(c).FillWidth().Gap(4).Children(func() {
			ui.Row(c).FillWidth().Gap(8).Children(func() {
				v.nodeControls(c)
				if ui.Button(c, v.tr("Refresh")).Disabled(v.locked() || v.accountPending || v.actions.RefreshCenter == nil).Clicked() {
					v.refreshCenter()
				}
			})
			if v.state.Running {
				if ui.Button(c, v.tr("End reception")).FillWidth().Disabled(v.stopping || v.state.Stopping || v.actions.EndReception == nil).Clicked() {
					v.stopping = true
					v.actions.EndReception()
				}
			} else if ui.PrimaryButton(c, v.tr("Start reception")).FillWidth().Disabled(!v.ready() || !s.LoggedIn || v.node == "" || v.saved.Operator.Bind == "" || v.accountPending || v.actions.StartReception == nil).Clicked() {
				v.listening = true
				v.actions.StartReception(v.node)
			}
		})
	})
	ui.Text(c, v.tr("Invite Client")).Bold().FontSize(12)
	ui.Row(c).FillWidth().Gap(8).AlignItems(ui.Center).Children(func() {
		code := ""
		if v.actions.CenterDisplay != nil && s.Phase == "open" && time.Now().Before(s.CodeExpiresAt) {
			code, _ = v.actions.CenterDisplay()
		}
		display := "---- ----"
		if len(code) == 8 {
			display = code[:4] + " " + code[4:]
		}
		ui.Text(c, display).Font(metricFontFamily).FontWeight(700).FontSize(24)
		if ui.Button(c, v.tr("Copy numeric code")).Disabled(len(code) != 8 || v.actions.Copy == nil).Clicked() {
			v.actions.Copy(code)
		}
		if s.PeerShort != "" {
			ui.Text(c, v.tr("Redeemed · visitor %s", s.PeerShort))
		} else if len(code) == 8 {
			v.countdown(c, s.CodeExpiresAt)
			ui.Text(c, v.tr("Attempts left: %d", s.AttemptsLeft))
		} else {
			ui.Text(c, v.tr("Client only needs these 8 digits."))
		}
	})
}

func (v *View) countdown(c *ui.Context, deadline time.Time) {
	left := max(0, int(time.Until(deadline).Round(time.Second)/time.Second))
	ui.Text(c, v.tr("Expires in %s", fmt.Sprintf("%d:%02d", left/60, left%60))).FontSize(12)
	c.After(time.Second)
}

func sasDigits(input string) (string, bool) {
	var b strings.Builder
	for _, r := range input {
		switch {
		case r == ' ' || r == '　':
			continue
		case r >= '０' && r <= '９':
			r = '0' + r - '０'
		case r < '0' || r > '9':
			return "", false
		}
		b.WriteByte(byte(r))
	}
	return b.String(), b.Len() <= 6
}

func (v *View) sasDialog(c *ui.Context) {
	s := v.state.Center
	if s.SASID == 0 || v.actions.CenterDisplay == nil {
		return
	}
	_, prompt := v.actions.CenterDisplay()
	if prompt.ID != s.SASID {
		return
	}
	if !time.Now().Before(s.SASDeadline) {
		return
	}
	open := true
	w, _ := c.Size()
	ui.Modal(c.Key(s.SASID), &open, func() {
		ui.Text(c, v.tr("Verify the short code")).Bold()
		client := v.role == settings.RoleClient
		confirm := "Confirm"
		enabled := false
		if client {
			ui.Text(c, v.tr("Read these 6 digits to the operator."))
			if len(prompt.Code) == 6 {
				ui.Text(c, prompt.Code[:3]+" "+prompt.Code[3:]).Font(metricFontFamily).FontSize(24).FontWeight(700)
				enabled = true
			}
			confirm = "Confirm (read to operator)"
		} else {
			ui.Text(c, v.tr("Ask the customer to read the 6 digits on their screen and enter them below. You can confirm only when they match.")).FillWidth()
			input := v.sasInput
			ui.TextInput(c.Key("sas"), &input).Label(v.tr("Short code")).Font(metricFontFamily).FontSize(24).FillWidth()
			if input != v.sasInput {
				digits, ok := sasDigits(input)
				if ok {
					v.sasInput = digits
					if len(digits) > 3 {
						v.sasInput = digits[:3] + " " + digits[3:]
					}
				} else {
					v.sasInput = ""
				}
				v.sasMatched = v.actions.MatchSAS != nil && v.actions.MatchSAS(s.SASID, v.sasInput)
			}
			enabled = v.sasMatched
			if len(v.sasInput) == 7 && !enabled {
				ui.Text(c, v.tr("Does not match this screen")).TextColor(amber)
			}
		}
		if s.PeerShort != "" {
			v.value(c, "Peer", s.PeerShort)
		}
		v.countdown(c, s.SASDeadline)
		layout := ui.Row
		if w < 360 {
			layout = ui.Column
		}
		layout(c).FillWidth().Gap(8).Children(func() {
			if ui.PrimaryButton(c, v.tr(confirm)).Disabled(!enabled || v.sasPending || v.actions.AnswerSAS == nil).Clicked() {
				v.sasPending = true
				v.actions.AnswerSAS(s.SASID, true)
			}
			if ui.Button(c, v.tr("Mismatch")).Disabled(v.sasPending || v.actions.AnswerSAS == nil).Clicked() {
				v.sasPending = true
				v.actions.AnswerSAS(s.SASID, false)
			}
		})
	}).Width(min(420, w-24)).MaxWidth(w-24).Padding(12).Gap(8).Border(1, line).Background(panel).Radius(0).NoShadow()
}
