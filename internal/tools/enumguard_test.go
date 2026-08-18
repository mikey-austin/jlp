package tools

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoSchemaDeclaresAnEmptyEnumMember guards a failure mode with a
// blast radius far larger than the mistake.
//
// Gemini rejects the WHOLE request when any enum member is empty:
//
//	GenerateContentRequest.tools[0].function_declarations[8]
//	  .parameters.properties[filter].enum[0]: cannot be empty
//
// Not that one tool — the whole request. So a single `"enum": ["", …]`
// anywhere in the registry took down every agentic call routed to
// Gemini, including the A2A chat, with an error naming a tool index
// rather than a tool. get_vocabulary_history spelled "everything" as ""
// and did exactly that.
//
// It is also poor prompting: an unnamed default is something the model
// has to infer from prose. Name the value.
func TestNoSchemaDeclaresAnEmptyEnumMember(t *testing.T) {
	var checked int

	// Every tool the registry can expose.
	for _, tool := range allToolsForSchemaCheck() {
		var schema any
		if err := json.Unmarshal(tool.Def.Schema, &schema); err != nil {
			t.Errorf("tool %s has an unparseable schema: %v", tool.Def.Name, err)
			continue
		}
		checked++
		walkForEmptyEnum(t, "tool "+tool.Def.Name, schema)
	}
	if checked == 0 {
		t.Fatal("checked no tool schemas; the scan has drifted from the registry")
	}

	// And the structured-output schemas, which travel the same path.
	dir := schemaDefsDir(t)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read schema defs: %v", err)
	}
	var files int
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var schema any
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Errorf("%s is not valid JSON: %v", e.Name(), err)
			continue
		}
		files++
		walkForEmptyEnum(t, e.Name(), schema)
	}
	if files == 0 {
		t.Fatal("checked no schema files")
	}
	t.Logf("checked %d tool schemas and %d schema files", checked, files)
}

func walkForEmptyEnum(t *testing.T, where string, node any) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		if raw, ok := v["enum"]; ok {
			if members, ok := raw.([]any); ok {
				for i, m := range members {
					if s, ok := m.(string); ok && s == "" {
						t.Errorf("%s: enum[%d] is the empty string. Gemini rejects the entire request for this, not just this field, so one of these disables every agentic call. Give the value a name (e.g. \"all\") and map it back where it is consumed.", where, i)
					}
				}
			}
		}
		for k, child := range v {
			walkForEmptyEnum(t, where+"."+k, child)
		}
	case []any:
		for _, child := range v {
			walkForEmptyEnum(t, where, child)
		}
	}
}

func schemaDefsDir(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	// <repo>/internal/tools/enumguard_test.go -> <repo>/internal/schemas/defs
	return filepath.Join(filepath.Dir(thisFile), "..", "schemas", "defs")
}

// allToolsForSchemaCheck builds every tool this package can register.
//
// The services are nil: a schema is a literal on the Tool, so
// constructing one touches nothing, and no handler is ever invoked here.
// Enumerating the constructors rather than a hand-written list of names
// is what makes a NEW tool covered the day it is written.
func allToolsForSchemaCheck() []Tool {
	var out []Tool
	out = append(out, WritingTools(nil)...)
	out = append(out, LearnerTools(nil, nil, nil)...)
	out = append(out, AnalyticsTools(nil, nil)...)
	out = append(out, VocabularyTools(nil)...)
	out = append(out, SessionTools(nil)...)
	out = append(out, LearningTools(nil, nil, nil, nil)...)
	return out
}
