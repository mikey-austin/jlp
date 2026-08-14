// Package schemas is the registry of JSON Schemas that AI responses
// must conform to. Each schema is embedded data under defs/<name>.json
// (versioned in its name, e.g. "correction_result.v1") and compiled
// exactly once, on first use, into a santhosh-tekuri/jsonschema/v6
// validator shared by every caller.
package schemas

import (
	"embed"
	"fmt"
	"strings"
	"sync"

	"encoding/json"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

//go:embed defs/*.json
var defsFS embed.FS

// resourceURL is an arbitrary, never-dereferenced URL used to register
// a schema's decoded document with the compiler by name.
const resourceURL = "mem://jlp/schemas/"

var (
	compileOnce sync.Once
	compiled    map[string]*jsonschema.Schema
	compileErr  error
)

// Get returns the raw embedded schema document for name (e.g.
// "correction_result.v1"), unparsed.
func Get(name string) (json.RawMessage, error) {
	raw, err := defsFS.ReadFile(defPath(name))
	if err != nil {
		return nil, fmt.Errorf("schemas: unknown schema %q: %w", name, err)
	}
	return json.RawMessage(raw), nil
}

// Validate checks doc against the named schema, compiling the full
// registry once (sync.Once) and caching the result across calls.
func Validate(name string, doc []byte) error {
	compileAll()
	if compileErr != nil {
		return fmt.Errorf("schemas: compile: %w", compileErr)
	}
	sch, ok := compiled[name]
	if !ok {
		return fmt.Errorf("schemas: unknown schema %q", name)
	}
	inst, err := jsonschema.UnmarshalJSON(strings.NewReader(string(doc)))
	if err != nil {
		return fmt.Errorf("schemas: doc is not valid JSON: %w", err)
	}
	if err := sch.Validate(inst); err != nil {
		return fmt.Errorf("schemas: %s: %w", name, err)
	}
	return nil
}

func compileAll() {
	compileOnce.Do(func() {
		entries, err := defsFS.ReadDir("defs")
		if err != nil {
			compileErr = err
			return
		}
		c := jsonschema.NewCompiler()
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			name := strings.TrimSuffix(e.Name(), ".json")
			raw, err := defsFS.ReadFile(defPath(name))
			if err != nil {
				compileErr = err
				return
			}
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(raw)))
			if err != nil {
				compileErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			if err := c.AddResource(resourceURL+name, doc); err != nil {
				compileErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			names = append(names, name)
		}
		compiled = make(map[string]*jsonschema.Schema, len(names))
		for _, name := range names {
			sch, err := c.Compile(resourceURL + name)
			if err != nil {
				compileErr = fmt.Errorf("%s: %w", name, err)
				return
			}
			compiled[name] = sch
		}
	})
}

func defPath(name string) string {
	return "defs/" + name + ".json"
}
