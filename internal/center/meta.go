// Package center checks public metadata without authenticating to a center.
package center

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"remote-enet/internal/settings"
)

const maxMetaBody = 16 * 1024

var centerIDPattern = regexp.MustCompile(`^[a-z0-9.-]{1,32}$`)

const (
	devVersion    = "0.0.0-dev"
	versionNumber = `(?:0|[1-9][0-9]*)`
	prereleaseID  = `(?:` + versionNumber + `|[0-9]*[A-Za-z-][0-9A-Za-z-]*)`
	buildID       = `[0-9A-Za-z-]+`
)

var semverPattern = regexp.MustCompile(`^` + versionNumber + `\.` + versionNumber + `\.` + versionNumber +
	`(?:-` + prereleaseID + `(?:\.` + prereleaseID + `)*)?(?:\+` + buildID + `(?:\.` + buildID + `)*)?$`)

// Meta contains the public fields from /v1/meta. ServerTime is decoded from Unix seconds.
type Meta struct {
	CenterID      string
	APIVersions   []string
	ServerTime    time.Time
	MinAppVersion MinAppVersion
}

// MinAppVersion carries the center's advertised version floor per role.
type MinAppVersion struct {
	Operator string
	Client   string
	Hub      string
}

// HTTPError exposes the machine-readable error code separately from log text.
type HTTPError struct {
	StatusCode int
	Code       string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("center: meta returned HTTP %d", e.StatusCode)
}

// FetchMeta requires a TLS-verifying transport without credentials and a client or operator role.
// The caller controls the deadline. Non-SemVer builds advertise 0.0.0-dev.
func FetchMeta(ctx context.Context, client *http.Client, entry, role, appVersion string) (Meta, error) {
	if role != settings.RoleClient && role != settings.RoleOperator {
		return Meta{}, fmt.Errorf("center: meta requires a client or operator role")
	}
	if !semverPattern.MatchString(appVersion) {
		appVersion = devVersion
	}
	u, err := url.Parse(entry)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		u.ForceQuery || u.RawQuery != "" || strings.Contains(entry, "#") {
		return Meta{}, fmt.Errorf("center: invalid HTTPS entry")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(entry, "/")+"/v1/meta", nil)
	if err != nil {
		return Meta{}, fmt.Errorf("center: build meta request: %w", err)
	}
	req.Header.Set("User-Agent", "Remote-ENET")
	req.Header.Set("Renet-Client", role+"/"+appVersion)
	probe := *client
	probe.Jar = nil
	probe.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := probe.Do(req)
	if err != nil {
		return Meta{}, fmt.Errorf("center: request meta: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetaBody+1))
	if err != nil {
		return Meta{}, fmt.Errorf("center: read meta response: %w", err)
	}
	if len(body) > maxMetaBody {
		return Meta{}, fmt.Errorf("center: meta response exceeds %d bytes", maxMetaBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		httpErr := &HTTPError{StatusCode: resp.StatusCode}
		if json.Unmarshal(body, &envelope) == nil {
			httpErr.Code = envelope.Error.Code
		}
		return Meta{}, httpErr
	}
	return decodeMeta(body)
}

func decodeMeta(body []byte) (Meta, error) {
	var wire struct {
		CenterID      string    `json:"center_id"`
		APIVersions   []*string `json:"api_versions"`
		ServerTime    *int64    `json:"server_time"`
		MinAppVersion struct {
			Operator *string `json:"operator"`
			Client   *string `json:"client"`
			Hub      *string `json:"hub"`
		} `json:"min_app_version"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		// Decoder errors may include values from the response.
		return Meta{}, fmt.Errorf("center: invalid meta JSON or field type")
	}
	if !centerIDPattern.MatchString(wire.CenterID) {
		return Meta{}, fmt.Errorf("center: invalid or missing center_id")
	}
	minApp := wire.MinAppVersion
	if wire.APIVersions == nil || wire.ServerTime == nil ||
		minApp.Operator == nil || minApp.Client == nil || minApp.Hub == nil {
		return Meta{}, fmt.Errorf("center: missing or null required meta field")
	}
	versions := make([]string, len(wire.APIVersions))
	for i, version := range wire.APIVersions {
		if version == nil {
			return Meta{}, fmt.Errorf("center: null api_versions element")
		}
		versions[i] = *version
	}
	return Meta{
		CenterID:    wire.CenterID,
		APIVersions: versions,
		ServerTime:  time.Unix(*wire.ServerTime, 0).UTC(),
		MinAppVersion: MinAppVersion{
			Operator: *minApp.Operator,
			Client:   *minApp.Client,
			Hub:      *minApp.Hub,
		},
	}, nil
}
