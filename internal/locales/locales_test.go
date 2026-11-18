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
