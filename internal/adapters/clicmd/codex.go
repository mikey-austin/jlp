package clicmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const codexProvider = "codexcli"

// extractCodexTrailingLine returns the text to run the shared
// balanced-JSON step (clicmd.go's GenerateStructured) against, from
// `codex exec --json`'s stdout.
//
// `--json` prints codex's protocol EVENTS as JSONL (confirmed via
// `codex exec --help`: "Print events to stdout as JSONL") — the
// trailing line is not guaranteed to BE the bare answer text on its
// own, despite the task brief's simplified framing ("stdout the raw
// text, last line JSON"); it may instead be an event envelope
// (e.g. `{"type":"item.completed","item":{...,"text":"<answer>"}}`)
// wrapping the answer inside one of its own string fields. This
// adapter has no live-tested transcript to pin an exact schema
// against — event field names vary across Codex CLI versions, and the
// host-mode-only design (PRD §23, see the package doc comment) means
// this path is never exercised against the real binary in
// CI/containers. Rather than hardcode a guessed field name (wrong for
// a different CLI version = silently broken), findNestedJSONObject
// searches the decoded line's string-typed fields, at any depth,
// schema-agnostically, for the first one that itself contains a
// balanced {...} block — correct whether that field is called `text`,
// `message`, `content`, or anything else.
//
// Falls back to the line's own raw bytes — the brief's literal "last
// line JSON" case, and what a bare (non-enveloped) answer, or a
// JSON-decode failure, both still exercise — when no such nested field
// is found.
func extractCodexTrailingLine(stdout []byte) ([]byte, error) {
	lines := bytes.Split(bytes.TrimRight(stdout, "\n"), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if nested, ok := findNestedJSONObject(line); ok {
			return nested, nil
		}
		return line, nil
	}
	return nil, fmt.Errorf("codex exec --json: empty output")
}

// findNestedJSONObject decodes line as a generic JSON value and
// searches its string-typed fields — objects and arrays, any depth —
// for one containing a balanced {...} block per
// aiutil.ExtractJSONObject. Map key order is sorted before iterating
// so the result is deterministic even though Go's own map iteration
// order isn't. Returns ok=false if line isn't JSON at all, or no
// string field inside it contains a balanced object.
func findNestedJSONObject(line []byte) ([]byte, bool) {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return nil, false
	}
	return searchJSONValue(v)
}

func searchJSONValue(v any) ([]byte, bool) {
	switch val := v.(type) {
	case string:
		return aiutil.ExtractJSONObject(val)
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if found, ok := searchJSONValue(val[k]); ok {
				return found, true
			}
		}
	case []any:
		for _, child := range val {
			if found, ok := searchJSONValue(child); ok {
				return found, true
			}
		}
	}
	return nil, false
}

// NewCodex returns an ai.StructuredGenerator that shells out to the
// OpenAI Codex CLI (`codex exec --json`, prompt on stdin) as a
// host-mode AI fallback (PRD §23). See the package doc comment for the
// host-mode-only caveat: cfg.Bin not being installed is not a
// construction-time error, only a per-call one.
func NewCodex(cfg config.CodexCLI) ai.StructuredGenerator {
	return &generator{
		bin:      cfg.Bin,
		args:     []string{"exec", "--json"},
		provider: codexProvider,
		extract:  extractCodexTrailingLine,
	}
}
