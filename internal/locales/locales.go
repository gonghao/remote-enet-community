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
	"unavailable":       "Code unavailable",
	"expired":           "Code expired",
	"budget_exhausted":  "Attempts exhausted",
	"rate_limited":      "Try again later",
	"busy":              "Operator busy",
	"confirmation":      "Incorrect code or connection tampered",
	"store_failed":      "Could not save pairing",
	"dns":               "Address lookup failed",
	"timeout":           "Connection timed out",
	"unsupported":       "Numeric pairing unsupported",
	"identity_mismatch": "Device identity mismatch",
	"interrupted":       "Pairing interrupted",
}

// PairingMessage accepts only fixed reason codes.
func PairingMessage(reason string) string { return pairingMessages[reason] }

// RuntimeErrorText translates application-owned messages among runtime diagnostics.
func RuntimeErrorText(tag, message string) string {
	switch message {
	case "Client disconnected.", "Client connection could not be accepted.", "Client session ended with an error.",
		"Operator disconnected.", "Operator session ended with an error.",
		"Pairing rejected.", "Incompatible protocol version.", "Runtime stopped with an error. Check runtime diagnostics.":
		return Text(tag, message)
	}
	for _, key := range pairingMessages {
		if message == key {
			return Text(tag, key)
		}
	}
	return message
}
