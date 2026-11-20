package centercode

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestCodeConformance(t *testing.T) {
	for _, vector := range []struct {
		id, file, section string
		normalize         func(string) (string, error)
		display           func(string) string
	}{
		{"V1", "v01-account-code.json", "4.1", NormalizeAccountCode, FormatAccountCode},
		{"V2", "v02-numeric-code.json", "4.2", NormalizeNumericCode, nil},
	} {
		t.Run(vector.id, func(t *testing.T) {
			data, err := os.ReadFile("../../testdata/conformance/" + vector.file)
			if err != nil {
				t.Fatal(err)
			}
			var doc struct {
				Format int
				Vector string
				Cases  []struct {
					Name   string
					Input  string
					Expect struct {
						Result     string
						Reason     string
						Normalized string
						Display    string
					}
				}
			}
			if err := json.Unmarshal(data, &doc); err != nil {
				t.Fatal(err)
			}
			if doc.Format != 1 || doc.Vector != vector.id || len(doc.Cases) == 0 {
				t.Fatal("invalid vector document")
			}
			// Both published vectors must exercise the White_Space boundaries.
			var accepted, rejected strings.Builder
			for _, tc := range doc.Cases {
				switch tc.Expect.Result {
				case "accept":
					accepted.WriteString(tc.Input)
				case "reject":
					if tc.Expect.Reason == vector.section+"/character" {
						rejected.WriteString(tc.Input)
					}
				}
			}
			for _, r := range "\u0085\u3000" {
				if !strings.ContainsRune(accepted.String(), r) {
					t.Errorf("missing accepted separator %U", r)
				}
			}
			for _, r := range "\ufeff\u200b\x1c\x1d\x1e\x1f" {
				if !strings.ContainsRune(rejected.String(), r) {
					t.Errorf("missing rejected symbol %U", r)
				}
			}
			for _, tc := range doc.Cases {
				t.Run(tc.Name, func(t *testing.T) {
					got, err := vector.normalize(tc.Input)
					switch tc.Expect.Result {
					case "accept":
						if err != nil || got != tc.Expect.Normalized {
							t.Fatalf("normalize = %q, %v; want %q", got, err, tc.Expect.Normalized)
						}
					case "reject":
						want := map[string]error{
							vector.section + "/length":    ErrCodeLength,
							vector.section + "/character": ErrCodeCharacter,
						}[tc.Expect.Reason]
						if want == nil || got != "" || !errors.Is(err, want) {
							t.Fatalf("normalize = %q, %v; want %s", got, err, tc.Expect.Reason)
						}
					default:
						t.Fatalf("unknown result %q", tc.Expect.Result)
					}
					if vector.display != nil {
						if display := vector.display(tc.Input); display != tc.Expect.Display {
							t.Errorf("display = %q; want %q", display, tc.Expect.Display)
						}
					}
				})
			}
		})
	}
}

func TestNormalizationErrors(t *testing.T) {
	for _, tc := range []struct {
		name      string
		normalize func(string) (string, error)
		input     string
		want      error
	}{
		{"account-length", NormalizeAccountCode, "ABCD-EFG", ErrCodeLength},
		{"account-character", NormalizeAccountCode, "ABCD-EFGU", ErrCodeCharacter},
		{"numeric-length", NormalizeNumericCode, "1234 56789", ErrCodeLength},
		{"numeric-character", NormalizeNumericCode, "1234 567A", ErrCodeCharacter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.normalize(tc.input); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v; want %v", err, tc.want)
			}
		})
	}
}
