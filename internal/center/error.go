package center

import (
	jsonwire "encoding/json"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"time"
)

// ErrUnreachable marks exhaustion of the configured entries or retry budget.
var ErrUnreachable = errors.New("center unreachable")

// Class determines whether Do may repeat a request.
type Class uint8

const (
	ClassBusiness Class = iota
	ClassRetry
	ClassWait
	ClassFix
)

// ClassOf treats unknown envelope codes as terminal.
func ClassOf(code string) Class {
	switch code {
	case "internal", "unavailable":
		return ClassRetry
	case "rate_limited", "quota_releasing", "session_not_ready":
		return ClassWait
	case "clock_skew", "nonce_reused":
		return ClassFix
	default:
		return ClassBusiness
	}
}

// Error preserves structured details for role-specific callers. Message text is discarded.
type Error struct {
	Status     int
	Code       string
	Class      Class
	RetryAfter time.Duration
	Details    jsonwire.RawMessage
}

func (e *Error) Error() string { return fmt.Sprintf("center: HTTP %d (%s)", e.Status, e.Code) }

func (e *Error) detailString(key string) string {
	var details map[string]jsonwire.RawMessage
	var value string
	if json.Unmarshal(e.Details, &details) == nil {
		_ = json.Unmarshal(details[key], &value)
	}
	return value
}

// EndReason is empty when details omit the session end reason.
func (e *Error) EndReason() string { return e.detailString("end_reason") }

// State is empty when details omit the current session state.
func (e *Error) State() string { return e.detailString("state") }

// ServerTime reports a valid nonnegative Unix timestamp from details.
func (e *Error) ServerTime() (time.Time, bool) {
	var d struct {
		Time *int64 `json:"server_time"`
	}
	if json.Unmarshal(e.Details, &d) != nil || d.Time == nil || *d.Time < 0 {
		return time.Time{}, false
	}
	return time.Unix(*d.Time, 0).UTC(), true
}

func responseError(status int, b []byte) *Error {
	var wire struct {
		Error struct {
			Code    string              `json:"code"`
			Retry   jsonwire.RawMessage `json:"retry_after_s"`
			Details jsonwire.RawMessage `json:"details"`
		} `json:"error"`
	}
	e := &Error{Status: status, Class: ClassRetry}
	if json.Unmarshal(b, &wire) != nil || wire.Error.Code == "" {
		return e
	}
	e.Code = wire.Error.Code
	e.Class = ClassOf(e.Code)
	e.Details = wire.Error.Details
	var seconds *int64
	if json.Unmarshal(wire.Error.Retry, &seconds) == nil && seconds != nil && *seconds >= 0 && *seconds <= math.MaxInt64/int64(time.Second) {
		e.RetryAfter = time.Duration(*seconds) * time.Second
	} else if e.Class == ClassWait {
		e.RetryAfter = -1
	}
	return e
}
