package config_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestSettingsCatalogueMatchesTheCodeAndTheREADMEBothWays diffs the settings the code defines (the
// exported Key constants of internal/config) against settings.catalogue.json and against the README's
// settings table, in both directions.
func TestSettingsCatalogueMatchesTheCodeAndTheREADMEBothWays(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("../../../internal/config/settings.catalogue.json")
	if err != nil {
		t.Fatal(err)
	}
	var catalogue struct {
		Settings []struct{ Key string } `json:"settings"`
	}
	if err := json.Unmarshal(data, &catalogue); err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, s := range catalogue.Settings {
		listed = append(listed, s.Key)
	}

	file, err := parser.ParseFile(token.NewFileSet(), "../../../internal/config/config.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var defined []string
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok || len(value.Names) != 1 || !strings.HasPrefix(value.Names[0].Name, "Key") || value.Names[0].Name == "KeySize" {
				continue
			}
			literal, ok := value.Values[0].(*ast.BasicLit)
			if !ok {
				t.Fatalf("constant %s is not a string literal", value.Names[0].Name)
			}
			text, err := strconv.Unquote(literal.Value)
			if err != nil {
				t.Fatal(err)
			}
			defined = append(defined, text)
		}
	}
	slices.Sort(listed)
	slices.Sort(defined)
	if !slices.Equal(listed, defined) {
		t.Errorf("the catalogue lists %v, the code defines %v", listed, defined)
	}

	readme, err := os.ReadFile("../../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range defined {
		if !strings.Contains(string(readme), "`"+key+"`") {
			t.Errorf("the README does not describe %s", key)
		}
	}
}
