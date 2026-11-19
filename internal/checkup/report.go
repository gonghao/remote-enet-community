package checkup

import (
	"encoding/json"
	"errors"
	"time"
)

var findingIDs = map[string]bool{
	"settings": true, "interfaces": true, "vehicle_adapter": true, "peer_configuration": true,
	"hub_dns": true, "hub_tcp": true, "hub_candidates": true, "presentation_adapter": true,
	"hub_port": true, "diagnostic_port": true, "control_port": true, "discovery_port": true,
	"public_endpoint": true, "mgu_permission": true, "mgu_routes": true, "peer_session": true,
}

var findingCodes = map[string]bool{
	"settings_invalid": true, "settings_valid": true, "interfaces_failed": true, "interfaces_available": true,
	"adapter_ambiguous": true, "adapter_unavailable": true, "adapter_selected": true,
	"pairing_missing": true, "pairing_invalid": true, "pairing_unavailable": true, "invite_invalid": true,
	"peer_available": true, "endpoint_invalid": true, "literal_address": true, "dns_resolved": true,
	"dns_empty": true, "endpoint_limit": true, "tcp_reachable": true, "socket_close_failed": true,
	"runtime_active": true, "port_available": true, "endpoint_missing": true, "remote_check_required": true,
	"permission_unobserved": true, "permission_available": true, "authorization_on_demand": true,
	"helper_missing": true, "administrator_required": true, "network_privilege_required": true,
	"mgu_not_discovered": true, "direct_active": true, "target_conflict": true, "local_conflict": true,
	"routes_unavailable": true, "routes_available": true, "not_running": true, "session_connected": true,
	"waiting_for_peer": true, "canceled": true, "probe_timeout": true, "permission_denied": true,
	"port_in_use": true, "address_unavailable": true, "connection_refused": true, "dns_failed": true,
	"network_failed": true,
}

// JSON projects only a bounded, fixed vocabulary. It is separate from the
// existing anonymous diagnostics schema and never serializes an Input.
func JSON(r Report) ([]byte, error) {
	if len(r.Findings) > 32 || r.FinishedAt.Before(r.StartedAt) {
		return nil, errors.New("checkup: invalid report")
	}
	type finding struct {
		ID         string `json:"id"`
		Index      int    `json:"index,omitempty"`
		Status     Status `json:"status"`
		Code       string `json:"code"`
		DurationMS int64  `json:"durationMs"`
	}
	result := struct {
		Schema     string    `json:"schema"`
		Version    int       `json:"version"`
		StartedAt  time.Time `json:"startedAt"`
		FinishedAt time.Time `json:"finishedAt"`
		Complete   bool      `json:"complete"`
		Canceled   bool      `json:"canceled"`
		TimedOut   bool      `json:"timedOut"`
		Findings   []finding `json:"findings"`
	}{Schema: "checkup/v1", Version: 1, StartedAt: r.StartedAt.UTC(), FinishedAt: r.FinishedAt.UTC(),
		Complete: r.Complete, Canceled: r.Canceled, TimedOut: r.TimedOut, Findings: make([]finding, 0, len(r.Findings))}
	for _, f := range r.Findings {
		if f.Duration < 0 {
			return nil, errors.New("checkup: invalid report")
		}
		id, code, status := f.ID, f.Code, f.Status
		if !findingIDs[id] {
			id = "unknown"
		}
		if _, direct := directInputStatus(code); !direct && !findingCodes[code] {
			code = "unknown"
		}
		switch status {
		case Pass, Warning, Failed, Skipped:
		default:
			status = "unknown"
		}
		index := f.Index
		if index < 0 || index > 4 {
			index = 0
		}
		result.Findings = append(result.Findings, finding{ID: id, Index: index, Status: status, Code: code, DurationMS: f.Duration.Milliseconds()})
	}
	return json.MarshalIndent(result, "", "  ")
}
