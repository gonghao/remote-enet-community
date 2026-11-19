package locales

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func TestCatalogsHaveCompleteMatchingFormats(t *testing.T) {
	verbs := regexp.MustCompile(`%[+\-# 0]*[0-9]*(?:\.[0-9]+)?[a-zA-Z%]`)
	for tag, messages := range catalogs {
		if len(messages) != len(catalogs["en"]) {
			t.Fatalf("incomplete %s catalog", tag)
		}
		for key, source := range catalogs["en"] {
			translated, ok := messages[key]
			if !ok || translated == "" || !slices.Equal(verbs.FindAllString(source, -1), verbs.FindAllString(translated, -1)) {
				t.Errorf("missing or incompatible message %s %q", tag, key)
			}
		}
	}
}

func TestPairingAndRuntimeMessagesAreCataloged(t *testing.T) {
	for _, key := range []string{
		"Generate numeric code", "Copy address and code", "Revoke code",
		"Expires in %s", "Attempts left: %d", "Completing pairing",
		"Invite or address:port and code", "Enter the direct address and port",
		"Invalid invite", "Invitation expired", "Invalid host", "Invalid port", "Enter eight digits",
		"Address lookup failed", "Connection timed out", "Operator busy",
		"Numeric pairing unsupported", "Device identity mismatch",
		"Code unavailable", "Code expired", "Code redeemed", "Attempts exhausted", "Try again later",
		"Code or peer confirmation failed", "Pairing interrupted", "Could not save pairing",
		"Client disconnected.", "Client connection could not be accepted.", "Client session ended with an error.",
		"Operator disconnected.", "Operator session ended with an error.",
		"Pairing rejected.", "Incompatible protocol version.", "Runtime stopped with an error. Check runtime diagnostics.",
		"Request a new numeric code.", "You can retry with the same code if it is still valid.",
		"Check the operator address before trying again.", "Try again later with a new numeric code.",
	} {
		for _, tag := range []string{"en", "zh-Hans", "zh-Hant"} {
			if catalogs[tag][key] == "" {
				t.Errorf("missing %s pairing or runtime message %q", tag, key)
			}
		}
	}
}

func TestRuntimeErrorTextUsesMessageWhitelist(t *testing.T) {
	message := PairingMessage("identity_mismatch")
	if got := RuntimeErrorText("zh-Hans", message); got != "设备身份不匹配" {
		t.Errorf("identity mismatch text: %q", got)
	}
	if got := RuntimeErrorText("zh-Hans", "Settings"); got != "Settings" {
		t.Errorf("unrelated diagnostic text was translated: %q", got)
	}
	for tag, want := range map[string]string{
		"en": "Client disconnected.", "zh-Hans": "客户端已断开连接。", "zh-Hant": "用戶端已中斷連線。",
	} {
		if got := RuntimeErrorText(tag, "Client disconnected."); got != want {
			t.Errorf("%s disconnect text: %q", tag, got)
		}
	}
}

func TestLanguageMatchingAndIsolation(t *testing.T) {
	for input, want := range map[string]string{"en-US": "en", "zh_CN.UTF-8": "zh-Hans", "zh-SG": "zh-Hans", "zh-TW": "zh-Hant", "zh-HK": "zh-Hant", "zh-Hant-TW": "zh-Hant", "ja-JP": "en", "": "en"} {
		if got := Match(input); got != want {
			t.Errorf("Match(%q)=%s, want %s", input, got, want)
		}
	}
	var wg sync.WaitGroup
	for tag, expected := range map[string]string{"en": "Settings", "zh-Hans": "设置", "zh-Hant": "設定"} {
		wg.Go(func() {
			for range 20 {
				if got := Text(tag, "Settings"); got != expected {
					t.Errorf("language leaked between views: %s", got)
				}
			}
		})
	}
	wg.Wait()
	if Text("zh-Hans", "not an application message") != "not an application message" {
		t.Fatal("unknown messages were modified")
	}
}

func TestLiteralDisplayMessagesAreCataloged(t *testing.T) {
	if _, err := os.Stat("../ui"); os.IsNotExist(err) {
		t.Skip("source coverage requires the repository beside this package")
	}
	// Numeric formats and example endpoints intentionally keep their technical notation.
	exceptions := map[string]bool{"%d / %d": true, "host.example:47000": true}
	for _, dir := range []string{"../ui", "../desktopui"} {
		paths, err := filepath.Glob(dir + "/*.go")
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range paths {
			if strings.HasSuffix(path, "_test.go") {
				continue
			}
			fs := token.NewFileSet()
			file, err := parser.ParseFile(fs, path, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok || len(call.Args) == 0 {
					return true
				}
				name := ""
				switch f := call.Fun.(type) {
				case *ast.SelectorExpr:
					name = f.Sel.Name
				case *ast.Ident:
					name = f.Name
				}
				if name != "tr" {
					return true
				}
				literal, ok := call.Args[0].(*ast.BasicLit)
				if !ok || literal.Kind != token.STRING {
					return true
				}
				source, _ := strconv.Unquote(literal.Value)
				if _, ok := catalogs["en"][source]; !ok && !exceptions[source] {
					t.Errorf("message missing from catalogs at %s: %q", fs.Position(literal.Pos()), source)
				}
				return true
			})
		}
	}
}
