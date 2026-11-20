// Package locales keeps display translations separate from protocol and log data.
package locales

import (
	"embed"
	"encoding/json"
	"fmt"

	"github.com/egoist/mygo/i18n"
)

//go:embed en.json zh-Hans.json zh-Hant.json
var files embed.FS

var catalogs = loadCatalogs()

func loadCatalogs() map[string]map[string]string {
	all := make(map[string]map[string]string, 3)
	for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
		data, err := files.ReadFile(tag + ".json")
		if err != nil {
			panic(err)
		}
		var messages map[string]string
		if err := json.Unmarshal(data, &messages); err != nil {
			panic(err)
		}
		all[tag] = messages
	}
	return all
}

// Match resolves regional and script variants, defaulting to English.
func Match(tag string) string {
	if matched := i18n.Match([]string{tag}, []string{"en", "zh-Hans", "zh-Hant"}); matched != "" {
		return matched
	}
	return "en"
}

// Text formats only an application-owned message. Callers must not pass raw
// identities, filenames, annotations or protocol payloads as message keys.
func Text(tag, source string, args ...any) string {
	text := source
	if translated, ok := catalogs[Match(tag)][source]; ok {
		text = translated
	}
	if len(args) != 0 {
		return fmt.Sprintf(text, args...)
	}
	return text
}

var pairingMessages = map[string]string{
	"hub_identity_mismatch":                "Hub identity mismatch",
	"hub_unreachable":                      "Cannot reach hub",
	"hub_wait_unauthorized":                "Hub has not authorized this device. Check the entry and the system time.",
	"hub_pairing_disabled":                 "Hub numeric pairing is not enabled for this device",
	"hub_authorization_expired_or_revoked": "Hub authorization expired or revoked",
	"operator_offline":                     "Operator offline",
	"hub_ticket":                           "Could not obtain hub authorization",
	"hub_issuer":                           "Issuer key on this device is unavailable. Check the state directory and file permissions.",
	"unavailable":                          "Code unavailable",
	"expired":                              "Code expired",
	"budget_exhausted":                     "Attempts exhausted",
	"rate_limited":                         "Try again later",
	"busy":                                 "Operator busy",
	"confirmation":                         "Incorrect code or connection tampered",
	"store_failed":                         "Could not save pairing",
	"dns":                                  "Address lookup failed",
	"timeout":                              "Connection timed out",
	"unsupported":                          "Numeric pairing unsupported",
	"identity_mismatch":                    "Device identity mismatch",
	"interrupted":                          "Pairing interrupted",
}

var centerMessages = map[string]string{
	"nic_missing":               "The selected vehicle-side IPv4 adapter is unavailable.",
	"unsupported":               "Numeric pairing unsupported",
	"bad_request":               "The center rejected the request format",
	"state_conflict":            "The current session state does not allow this action",
	"idempotency_key_reused":    "The request key was reused for a different operation",
	"forbidden":                 "This device does not have permission for this action",
	"peer_version_incompatible": "The peer protocol version is incompatible",
	"center_protocol_error":     "The center returned an invalid response",
	"not_found":                 "Action failed",
	"center_unconfigured":       "No center is configured. Set one in Settings, or enter a direct address and code.",
	"center_unreachable":        "Cannot reach the center",
	"center_id_mismatch":        "Center identity mismatch",
	"upgrade_required":          "Update the app to continue",
	"rate_limited":              "Too many requests. Try again later.",
	"account_code_format":       "Account code has an invalid character",
	"account_code_invalid":      "Account code not found",
	"account_disabled":          "Account disabled",
	"account_expired":           "Account authorization expired",
	"device_limit":              "Device limit reached. Revoke an old device first.",
	"device_busy":               "This device already has an open session",
	"unauthenticated":           "Sign-in expired. Enter the account code again.",
	"quota_exhausted":           "Concurrent session limit reached",
	"quota_releasing":           "Releasing the previous session. Retrying automatically.",
	"node_unavailable":          "Node unavailable. Choose another node.",
	"code_invalid":              "Numeric code invalid or expired",
	"operator_waiting":          "Waiting for the operator",
	"busy":                      "Operator busy",
	"hub_unauthorized":          "The hub refused the connection authorization",
	"awaiting_center":           "Waiting for center confirmation",
	"confirmation":              "Incorrect code or connection tampered",
	"operator_ended":            "Operator ended the session",
	"client_ended":              "Client ended the session",
	"rejected":                  "Short codes did not match. Session cancelled.",
	"confirm_timeout":           "Short code was not confirmed in time",
	"pake_failed":               "Too many failed attempts. Request a new code.",
	"pairing_lost":              "Pairing state lost. Request a new code.",
	"code_expired":              "Numeric code expired",
	"redeem_limit":              "Code voided by the redemption limit",
	"pair_timeout":              "Pairing was not completed in time",
	"idle_timeout":              "Session ended after a long disconnection",
	"device_revoked":            "Device revoked",
	"node_lost":                 "Node offline",
	"node_disabled":             "Node disabled",
	"admin_revoked":             "Session ended by the administrator",
}

// CenterMessage accepts only known center reasons and resolves role-specific recovery text.
func CenterMessage(reason, role string) string {
	if reason == "unauthenticated" && role == "client" {
		return "Authorization lost. Enter a new numeric code."
	}
	return centerMessages[reason]
}

// PairingMessage accepts only fixed reason codes.
func PairingMessage(reason string) string {
	if key := pairingMessages[reason]; key != "" {
		return key
	}
	return centerMessages[reason]
}

// RuntimeErrorText translates application-owned messages among runtime diagnostics.
func RuntimeErrorText(tag, message string) string {
	switch message {
	case "Client disconnected.", "Client connection could not be accepted.", "Client session ended with an error.",
		"Operator disconnected.", "Operator session ended with an error.",
		"Pairing rejected.", "Incompatible protocol version.", "Runtime stopped with an error. Check runtime diagnostics.":
		return Text(tag, message)
	}
	for _, key := range centerMessages {
		if message == key {
			return Text(tag, key)
		}
	}
	if message == "Authorization lost. Enter a new numeric code." {
		return Text(tag, message)
	}
	for _, key := range pairingMessages {
		if message == key {
			return Text(tag, key)
		}
	}
	return message
}
