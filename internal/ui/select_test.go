package ui

import (
	"embed"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"slices"
	"testing"
)

//go:embed capture.go forms.go language.go
var dropdownSources embed.FS

// Select's chevron is private to MyGo. Check the construction paths before
// rasterization: the same vector can cover different pixels at different origins.
func TestDropdownsUseThemedChevron(t *testing.T) {
	for _, tc := range []struct {
		file, function string
		want           []string
	}{
		{"language.go", "languageSelect", []string{"ui.Select"}},
		{"forms.go", "operatorForm", []string{"ui.Select"}},
		{"capture.go", "capturePage", []string{"ui.Select"}},
		{"forms.go", "nicSelect", []string{"ui.SelectBase", "selectChevron"}},
		{"forms.go", "selectChevron", []string{"StrokePath"}},
	} {
		t.Run(tc.function, func(t *testing.T) {
			src, err := dropdownSources.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			file, err := parser.ParseFile(token.NewFileSet(), tc.file, src, parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Name.Name != tc.function {
					continue
				}
				for node := range ast.Preorder(fn.Body) {
					call, ok := node.(*ast.CallExpr)
					if !ok {
						continue
					}
					if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "StrokePath" {
						got = append(got, "StrokePath")
						continue
					}
					switch name := types.ExprString(call.Fun); name {
					case "ui.Select", "ui.SelectBase", "selectChevron":
						got = append(got, name)
					}
				}
			}
			if !slices.Equal(got, tc.want) {
				t.Fatalf("dropdown construction in %s: got %v, want %v", tc.file, got, tc.want)
			}
		})
	}
}
