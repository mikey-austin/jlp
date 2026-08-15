package event_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/event"
)

// declaredTypeConstants parses event.go's own source (found relative
// to this test file, so it works regardless of working directory) and
// returns the string value of every `Name Type = "..."` constant it
// declares. This is what keeps event.AllTypes() honest: rather than
// trusting AllTypes' hand-curated list against nothing, the test below
// independently rediscovers the ground truth by reading the source
// file itself.
func declaredTypeConstants(t *testing.T) map[string]bool {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	src := filepath.Join(filepath.Dir(thisFile), "event.go")

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", src, err)
	}

	declared := map[string]bool{}
	for _, decl := range f.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			ident, ok := vs.Type.(*ast.Ident)
			if !ok || ident.Name != "Type" {
				// Skip any const in event.go that isn't explicitly typed
				// Type — event.go currently has none, but a future
				// unrelated const block must not confuse this parser.
				continue
			}
			for _, v := range vs.Values {
				lit, ok := v.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				val, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatalf("unquote %s: %v", lit.Value, err)
				}
				declared[val] = true
			}
		}
	}
	return declared
}

// TestAllTypesMatchesDeclaredConstants fails in EITHER direction: a
// Type constant declared in event.go but missing from AllTypes()
// (the drift this test exists to catch — e.g. a future task adds
// TypeFooBar and forgets to also add it to AllTypes()), or an
// AllTypes() entry that names no real constant (a typo, or a
// constant since removed). Confirms the parser itself found
// something too, so a change to event.go's syntax that silently broke
// declaredTypeConstants's assumptions can't pass by finding zero of
// both and calling it a match.
func TestAllTypesMatchesDeclaredConstants(t *testing.T) {
	declared := declaredTypeConstants(t)
	if len(declared) == 0 {
		t.Fatal("declaredTypeConstants found zero `X Type = \"...\"` constants in event.go — parser assumptions are wrong")
	}

	got := map[string]bool{}
	for _, ty := range event.AllTypes() {
		got[string(ty)] = true
	}

	var missingFromAllTypes, extraInAllTypes []string
	for v := range declared {
		if !got[v] {
			missingFromAllTypes = append(missingFromAllTypes, v)
		}
	}
	for v := range got {
		if !declared[v] {
			extraInAllTypes = append(extraInAllTypes, v)
		}
	}
	sort.Strings(missingFromAllTypes)
	sort.Strings(extraInAllTypes)

	if len(missingFromAllTypes) > 0 {
		t.Errorf("declared in event.go but missing from AllTypes(): %v", missingFromAllTypes)
	}
	if len(extraInAllTypes) > 0 {
		t.Errorf("in AllTypes() but not declared in event.go: %v", extraInAllTypes)
	}
}

func TestAllTypesHasNoDuplicates(t *testing.T) {
	seen := map[event.Type]bool{}
	for _, ty := range event.AllTypes() {
		if seen[ty] {
			t.Fatalf("duplicate Type %q in AllTypes()", ty)
		}
		seen[ty] = true
	}
}
