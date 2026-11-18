// Package diagnostics builds an explicitly requested, allowlisted diagnostic report.
package diagnostics

import (
	"encoding/json"
	"errors"
	"math"
	"regexp"
	"runtime"

	"remote-enet/internal/app"
)

// Permission contains observed capabilities, never inferred from a role or phase.
// Platform must match runtime.GOOS; an empty or unknown platform is rejected.
type Permission struct {
	Platform                                         string
	Elevated, HelperAvailable, AuthorizationRequired bool
}

// Input must remain unchanged by the caller for the duration of Build.
// Runtime collections are neither retained nor traversed.
type Input struct {
	Version, Role                             string
	Runtime                                   app.Snapshot
	Running, Stopping, Testing, DirectWorking bool
	Permission                                Permission
}

// Versions accept only release forms and the fixed preview build grammar.
var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})\.(0|[1-9][0-9]{0,8})(-(alpha|beta|rc)\.(0|[1-9][0-9]{0,8})|-preview\.(wave[0-9]{1,3}(\.[0-9]{1,3})?|[0-9a-f]{7,40})(-worktree)?)?$`)

type report struct {
	SchemaVersion int              `json:"schemaVersion"`
	Version       string           `json:"version"`
	System        systemReport     `json:"system"`
	Role          string           `json:"role"`
	Runtime       runtimeReport    `json:"runtime"`
	Lifecycle     lifecycleReport  `json:"lifecycle"`
	Permission    permissionReport `json:"permission"`
}
type systemReport struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}
type permissionReport struct {
	Platform              string `json:"platform"`
	Elevated              bool   `json:"elevated"`
	HelperAvailable       bool   `json:"helperAvailable"`
	AuthorizationRequired bool   `json:"authorizationRequired"`
}
type lifecycleReport struct {
	Running       bool `json:"running"`
	Stopping      bool `json:"stopping"`
	Testing       bool `json:"testing"`
	DirectWorking bool `json:"directWorking"`
}
type runtimeReport struct {
	Role           string          `json:"role"`
	Phase          string          `json:"phase"`
	Generation     uint64          `json:"generation"`
	Path           string          `json:"path"`
	HasError       bool            `json:"hasError"`
	RTTNanoseconds int64           `json:"rttNanoseconds"`
	RTTValid       bool            `json:"rttValid"`
	Metrics        metricsReport   `json:"metrics"`
	Exclusive      exclusiveReport `json:"exclusive"`
	Direct         directReport    `json:"direct"`
}
type directReport struct {
	Phase    string `json:"phase"`
	HasError bool   `json:"hasError"`
}
type exclusiveReport struct {
	Enabled bool `json:"enabled"`
	Active  int  `json:"active"`
}
type metricsReport struct {
	Incomplete            bool    `json:"incomplete"`
	ActiveConnections     int     `json:"activeConnections"`
	HasError              bool    `json:"hasError"`
	TotalIn               uint64  `json:"totalIn"`
	TotalOut              uint64  `json:"totalOut"`
	SessionGeneration     uint64  `json:"sessionGeneration"`
	SessionActive         bool    `json:"sessionActive"`
	SessionIn             uint64  `json:"sessionIn"`
	SessionOut            uint64  `json:"sessionOut"`
	InBytesPerSecond      float64 `json:"inBytesPerSecond"`
	OutBytesPerSecond     float64 `json:"outBytesPerSecond"`
	RateWindowNanoseconds int64   `json:"rateWindowNanoseconds"`
	RTTSamples            int     `json:"rttSamples"`
	RTTTotalSamples       uint64  `json:"rttTotalSamples"`
	RTTLatestNanoseconds  int64   `json:"rttLatestNanoseconds"`
	RTTP50Nanoseconds     int64   `json:"rttP50Nanoseconds"`
	RTTP95Nanoseconds     int64   `json:"rttP95Nanoseconds"`
}

// Build returns an independent JSON schema v1 report of at most 64 KiB.
// Errors have fixed text and always return nil bytes. No external state is read.
func Build(in Input) ([]byte, error) {
	if len(in.Version) > 64 || (in.Version != "dev" && !versionPattern.MatchString(in.Version)) {
		return nil, errors.New("diagnostics: invalid version")
	}
	if in.Permission.Platform != runtime.GOOS {
		return nil, errors.New("diagnostics: permission platform mismatch")
	}
	s := in.Runtime
	m := s.Metrics
	if math.IsNaN(m.Rate.InBytesPerSecond) || math.IsInf(m.Rate.InBytesPerSecond, 0) || math.IsNaN(m.Rate.OutBytesPerSecond) || math.IsInf(m.Rate.OutBytesPerSecond, 0) {
		return nil, errors.New("diagnostics: non-finite rate")
	}
	r := report{
		SchemaVersion: 1, Version: in.Version,
		System:     systemReport{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH},
		Role:       normalize(in.Role, "client", "operator"),
		Permission: permissionReport{Platform: runtime.GOOS, Elevated: in.Permission.Elevated, HelperAvailable: in.Permission.HelperAvailable, AuthorizationRequired: in.Permission.AuthorizationRequired},
		Lifecycle:  lifecycleReport{in.Running, in.Stopping, in.Testing, in.DirectWorking},
		Runtime: runtimeReport{
			Role:       normalize(string(s.Role), "client", "operator"),
			Phase:      normalize(string(s.Phase), "idle", "starting", "waiting-nic", "discovering", "waiting-vehicle", "connecting", "listening", "connected", "disconnected", "backoff", "stopped"),
			Generation: s.Generation,
			Path:       normalize(string(s.Path.Kind), "direct", "relay"),
			HasError:   s.LastError != "", RTTValid: s.RTTValid,
			Exclusive: exclusiveReport{Enabled: s.Exclusive.Enabled, Active: s.Exclusive.Active},
			Direct:    directReport{Phase: normalize(string(s.Direct.Phase), "disabled", "discovering", "starting", "ready", "stopping", "failed"), HasError: s.Direct.LastError != ""},
			Metrics: metricsReport{
				Incomplete: s.MetricsIncomplete, ActiveConnections: m.ActiveConnections, HasError: m.LastError != nil,
				TotalIn: m.Totals.In, TotalOut: m.Totals.Out,
				SessionGeneration: m.Session.Generation, SessionActive: m.Session.Active,
				SessionIn: m.Session.Bytes.In, SessionOut: m.Session.Bytes.Out,
				InBytesPerSecond: m.Rate.InBytesPerSecond, OutBytesPerSecond: m.Rate.OutBytesPerSecond, RateWindowNanoseconds: int64(m.Rate.Window),
				RTTSamples: m.Session.RTT.Samples, RTTTotalSamples: m.Session.RTT.TotalSamples,
			},
		},
	}
	if s.RTTValid {
		r.Runtime.RTTNanoseconds = int64(s.RTT)
	}
	if m.Session.RTT.Samples > 0 {
		r.Runtime.Metrics.RTTLatestNanoseconds = int64(m.Session.RTT.Latest)
		r.Runtime.Metrics.RTTP50Nanoseconds = int64(m.Session.RTT.P50)
		r.Runtime.Metrics.RTTP95Nanoseconds = int64(m.Session.RTT.P95)
	}
	data, err := json.Marshal(r)
	if err != nil {
		return nil, errors.New("diagnostics: encode report")
	}
	if len(data) > 64*1024 {
		return nil, errors.New("diagnostics: report too large")
	}
	return data, nil
}

func normalize(value string, allowed ...string) string {
	for _, candidate := range allowed {
		if value == candidate {
			return candidate
		}
	}
	return "unknown"
}
