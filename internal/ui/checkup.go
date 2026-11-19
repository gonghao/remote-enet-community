package ui

import (
	"strings"

	"github.com/egoist/mygo/ui"

	"remote-enet/internal/checkup"
	"remote-enet/internal/settings"
)

func (v *View) checksEntry(c *ui.Context) {
	if ui.Button(c.Key("open-checks"), v.tr("Connection checks")).Label(v.tr("Open connection checks")).Clicked() {
		v.page = checksPage
	}
}

func (v *View) checksPage(c *ui.Context) {
	v.pageTitle(c, "Connection checks", "Local prerequisites · no vehicle requests")
	ui.Row(c).FillWidth().Gap(8).Children(func() {
		if ui.PrimaryButton(c.Key("run-checks"), v.tr("Run checks")).Label(v.tr("Run connection checks")).Grow(1).Height(36).
			Disabled(v.actions.CheckConnection == nil || v.checking || v.state.Checking || v.cancelingChecks || v.state.Stopping || v.submitted != nil || v.dirty()).Clicked() {
			v.checking = true
			v.actions.CheckConnection(strings.TrimSpace(v.invitation))
		}
		if ui.Button(c.Key("cancel-checks"), v.tr("Cancel checks")).Label(v.tr("Cancel connection checks")).Grow(1).Height(36).
			Disabled(v.actions.CancelChecks == nil || v.cancelingChecks || !v.checking && !v.state.Checking).Clicked() {
			v.cancelingChecks = true
			v.actions.CancelChecks()
		}
	})
	ui.Text(c, v.tr("Checks use saved settings. Save network changes first.")).FontSize(11).TextColor(muted)
	ui.Text(c, v.tr("TCP reachability does not verify identity, pairing or Internet access. Active listeners are not probed.")).FontSize(11).TextColor(muted)
	if v.dirty() {
		ui.Text(c, v.tr("Save changes in Settings before running checks.")).TextColor(amber).Role(ui.RoleStatus)
	}
	v.messages(c)
	report := v.state.Checkup
	status := "No checks have run for the current connection."
	switch {
	case v.checking || v.state.Checking:
		status = "Checking saved connection settings…"
	case report.Canceled:
		status = "Connection checks canceled."
	case report.TimedOut:
		status = "Connection checks timed out. Partial results are shown."
	case !report.StartedAt.IsZero():
		status = "Connection checks finished. Review warnings and skipped items."
	}
	ui.Text(c, v.tr(status)).FontSize(11).Role(ui.RoleStatus)
	if !report.StartedAt.IsZero() {
		ui.Text(c, v.tr("Observed at %s", report.StartedAt.Local().Format("15:04:05"))).FontSize(11).TextColor(muted)
		data, err := checkup.JSON(report)
		if ui.Button(c.Key("copy-checks"), v.tr("Copy check results")).Label(v.tr("Copy connection check report")).
			Disabled(v.actions.Copy == nil || v.checking || v.state.Checking || err != nil).Clicked() {
			v.actions.Copy(string(data))
		}
	}
	for i, f := range report.Findings {
		title := v.tr(checkTitle(f.ID))
		if f.Index > 0 && f.Index <= 4 {
			title = v.tr("%s · candidate %d", title, f.Index)
		}
		color := muted
		switch f.Status {
		case checkup.Pass:
			color = green
		case checkup.Warning:
			color = amber
		case checkup.Failed:
			color = c.Theme().Danger
		}
		v.card(c.Key(i), title, "", func() {
			ui.Text(c, v.tr("%s · %d ms", v.tr(checkStatus(f.Status)), max(0, f.Duration.Milliseconds()))).FontSize(11).Bold().TextColor(color)
			ui.Text(c, v.tr(checkHint(f.Code, v.role))).FontSize(11)
		})
	}
}

func checkStatus(s checkup.Status) string {
	switch s {
	case checkup.Pass:
		return "Passed"
	case checkup.Warning:
		return "Needs attention"
	case checkup.Failed:
		return "Failed"
	case checkup.Skipped:
		return "Skipped"
	default:
		return "Unknown"
	}
}

func checkTitle(id string) string {
	switch id {
	case "settings":
		return "Saved settings"
	case "interfaces":
		return "Network interfaces"
	case "vehicle_adapter":
		return "Vehicle-side adapter"
	case "peer_configuration":
		return "Peer configuration"
	case "hub_dns":
		return "Hub address resolution"
	case "hub_tcp":
		return "Hub TCP reachability"
	case "hub_candidates":
		return "Hub candidate limit"
	case "presentation_adapter":
		return "Presentation adapter"
	case "hub_port":
		return "Operator listening port"
	case "diagnostic_port":
		return "Diagnostic TCP port"
	case "control_port":
		return "Control TCP port"
	case "discovery_port":
		return "Discovery UDP port"
	case "public_endpoint":
		return "Public endpoint"
	case "mgu_permission":
		return "MGU network permission"
	case "mgu_routes":
		return "MGU route conflicts"
	case "peer_session":
		return "Current peer session"
	default:
		return "Unknown check"
	}
}

func checkHint(code, role string) string {
	switch code {
	case "settings_valid":
		return "Saved settings are valid."
	case "settings_invalid":
		return "Review and save the network settings."
	case "interfaces_available":
		return "Local network interfaces were read successfully."
	case "interfaces_failed":
		return "Network interfaces could not be read. Retry after checking system network settings."
	case "adapter_selected":
		return "The saved adapter selection is currently available."
	case "adapter_ambiguous":
		return "Several link-local adapters match. Choose a specific adapter and IPv4 on the session page."
	case "adapter_unavailable":
		if role == settings.RoleOperator {
			return "The presentation IPv4 is unavailable. Reconnect its adapter or update the address in Settings."
		}
		return "The saved adapter or IPv4 is missing. Reconnect the adapter, or choose another on the session page."
	case "peer_available":
		return "A pinned peer endpoint is available for reachability checks."
	case "pairing_missing":
		return "Paste an invitation on the main page, or connect once to save a paired peer."
	case "pairing_invalid", "pairing_unavailable":
		return "The saved peer could not be read. Retry with a current invitation."
	case "invite_invalid":
		return "The invitation is invalid or expired. Request a new invitation."
	case "endpoint_invalid":
		return "The endpoint is invalid. Check the saved endpoint or request a new invitation."
	case "literal_address":
		return "The endpoint uses an IP address; no DNS lookup is needed."
	case "dns_resolved":
		return "The endpoint hostname resolved successfully."
	case "dns_empty", "dns_failed":
		return "DNS resolution failed. Check the hostname and system DNS configuration."
	case "endpoint_limit":
		return "Only the first four Hub candidates were checked."
	case "tcp_reachable":
		return "TCP connected and closed without sending application data. Identity and pairing were not tested."
	case "socket_close_failed":
		return "The probe socket reported a close error. Retry the checks."
	case "runtime_active":
		return "Skipped to protect active listeners. Stop the runtime to check port availability."
	case "port_available":
		return "The port was briefly bound and released. This does not reserve it."
	case "endpoint_missing":
		return "Save a public endpoint before sharing an invitation."
	case "remote_check_required":
		return "The address is valid, but public reachability requires a test from another network."
	case "permission_unobserved":
		return "Network permission was not observed on this platform."
	case "permission_available":
		return "The process has elevated privileges. Device creation was not attempted."
	case "authorization_on_demand":
		return "The helper is available. macOS will request permission only when you enable MGU direct link."
	case "helper_missing":
		return "The network helper is unavailable. Use a complete Operator app package."
	case "administrator_required":
		return "MGU direct link requires running Operator as administrator on Windows."
	case "network_privilege_required":
		return "MGU device creation requires network privileges. This check does not test capabilities."
	case "mgu_not_discovered":
		return "No MGU address is known. No vehicle query was sent."
	case "direct_active":
		return "Skipped while MGU direct link owns or configures its routes."
	case "target_conflict":
		return "An existing local address or route covers the MGU target. Review VPN and adapter settings."
	case "local_conflict":
		return "All automatic local addresses conflict. Review local adapters and routes."
	case "routes_unavailable":
		return "Routes could not be inspected. Retry with the required system permissions."
	case "routes_available":
		return "No route conflict was found. No virtual adapter or route was created."
	case "not_running":
		return "No runtime is running. Start a session to observe peer connection status."
	case "session_connected":
		return "The peer session is currently connected. Vehicle operations were not tested."
	case "waiting_for_peer":
		return "The runtime is waiting for a peer. Review both endpoints and the connection status."
	case "canceled":
		return "The check was canceled."
	case "probe_timeout":
		return "The check timed out. Review connectivity and retry."
	case "permission_denied":
		return "The system denied the socket operation. Review system permissions and firewall rules."
	case "port_in_use":
		return "Another listener is using the port. Stop it or save a different port."
	case "address_unavailable":
		if role == settings.RoleOperator {
			return "The configured address is no longer available. Review the listen address and presentation IPv4 in Settings."
		}
		return "The configured address is no longer available. Refresh adapters and choose an available IPv4 on the session page."
	case "connection_refused":
		return "The endpoint refused TCP. Check the remote listener and port forwarding."
	case "network_failed":
		return "The network probe failed. Check local connectivity, firewall rules and the remote listener."
	default:
		return "An unexpected check result was received. Run the checks again."
	}
}
