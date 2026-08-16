package clicmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
// CI/containers.
//
// The discriminator: findNestedJSONObject only attempts extraction on
// a string value when its OWN map key is one of a fixed set of
// conventional message-carrying names (codexMessageFields, below) —
// it does NOT search every string field indiscriminately. An earlier
// version of this function did exactly that, and a code reviewer
// found a real corruption case for it: a LEGITIMATE top-level answer
// can itself contain a prose field (e.g. "explanation", or an
// exercise's own "instructions") that happens to quote a
// JSON-shaped example as part of its text — very plausible for an
// LLM's own output — and searching indiscriminately returns that
// quoted example instead of the real, complete answer. Restricting
// descent to conventional message-carrying names means a field
// like "explanation" is walked PAST (if it's a container, traversal
// still continues into it — see searchJSONValue) but its OWN string
// value is never handed to aiutil.ExtractJSONObject.
//
// This also means a schema whose answer happens to have its own
// top-level "type" field (e.g. exercise.v1's "multiple-choice" et al.)
// is NOT mistaken for a codex envelope: "type" isn't itself a
// message-carrying field, so its presence doesn't trigger descent —
// only an actual message-carrying field's string VALUE containing a
// balanced object does.
//
// Falls back to the line's own raw bytes — the brief's literal "last
// line JSON" case, and what a bare (non-enveloped) answer, a
// JSON-decode failure, or an object with no message-carrying field
// all still exercise — when no such nested field yields a balanced
// object.
// A live transcript (Codex CLI 0.135.0) since confirmed the enveloped
// shape this was written defensively for, and pinned the exact events:
//
//	{"type":"thread.started","thread_id":"…"}
//	{"type":"turn.started"}
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"{…answer…}"}}
//	{"type":"turn.completed","usage":{"input_tokens":14168,"cached_input_tokens":2432,"output_tokens":231,…}}
//
// Two consequences. The answer is NOT on the trailing line —
// turn.completed is — so the scan really does need to walk backwards
// past non-answer events rather than trusting the last line. And usage
// lives on that trailing event, so it must be collected on the way
// past. See codex_transcript_test.go, which runs the real bytes.
func extractCodexTrailingLine(stdout []byte) (cliOutput, error) {
	out := cliOutput{}
	lines := bytes.Split(bytes.TrimRight(stdout, "\n"), []byte("\n"))
	var answer []byte
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 {
			continue
		}
		if out.InputTokens == 0 && out.OutputTokens == 0 {
			if in, outTok, ok := codexUsage(line); ok {
				out.InputTokens, out.OutputTokens = in, outTok
				continue
			}
		}
		if answer != nil {
			continue
		}
		if nested, ok := findNestedJSONObject(line); ok {
			answer = nested
			continue
		}
		// A non-JSON trailing line is the bare-answer case: take it and
		// stop, since earlier lines are then not part of the answer.
		answer = line
		break
	}
	if answer == nil {
		return cliOutput{}, fmt.Errorf("codex exec --json: empty output")
	}
	out.Text = answer
	return out, nil
}

// codexUsage pulls token counts off a turn.completed event.
// cached_input_tokens is counted into the input total for the same
// reason claude.go sums its cache fields: they were really consumed,
// and omitting them makes the dashboard understate the request.
func codexUsage(line []byte) (input, output int, ok bool) {
	var ev struct {
		Type  string `json:"type"`
		Usage struct {
			InputTokens       int `json:"input_tokens"`
			CachedInputTokens int `json:"cached_input_tokens"`
			OutputTokens      int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(line, &ev); err != nil || ev.Type != "turn.completed" {
		return 0, 0, false
	}
	return ev.Usage.InputTokens + ev.Usage.CachedInputTokens, ev.Usage.OutputTokens, true
}

// codexMessageFields is the set of JSON object key names
// findNestedJSONObject treats as plausibly carrying the model's answer
// text, checked case-insensitively. None of these is confirmed against
// a real Codex CLI transcript (see extractCodexTrailingLine's doc
// comment on why not — this dev host's `codex` is authenticated, and a
// live call would incur real API cost with no clear authorization to
// spend it just to inspect output shape). They're deliberately generic
// names conventional across LLM-tool JSONL/event formats, not specific
// to one exact protocol version.
var codexMessageFields = map[string]bool{
	"msg": true, "message": true, "content": true, "text": true,
	"output": true, "last_agent_message": true, "result": true,
}

// findNestedJSONObject decodes line as a generic JSON value and
// searches it for a message-carrying field (codexMessageFields) whose
// string value itself contains a balanced {...} block per
// aiutil.ExtractJSONObject. Map key order is sorted before iterating
// so the result is deterministic even though Go's own map iteration
// order isn't. Returns ok=false if line isn't JSON at all, or no
// message-carrying field inside it yields a balanced object.
func findNestedJSONObject(line []byte) ([]byte, bool) {
	var v any
	if err := json.Unmarshal(line, &v); err != nil {
		return nil, false
	}
	return searchJSONValue(v)
}

// searchJSONValue recursively walks v. Containers (objects and arrays)
// are always traversed — a message-carrying field can be nested inside
// an intermediate object (e.g. "item") whose own key isn't itself
// message-carrying, so traversal can't stop at the first
// non-matching key. But a STRING value is only ever handed to
// aiutil.ExtractJSONObject when it's the direct value of a map key in
// codexMessageFields; every other string (an explanation, instructions,
// or any other prose field) is left alone entirely, never probed for
// embedded JSON, however brace-shaped its content might be.
func searchJSONValue(v any) ([]byte, bool) {
	switch val := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(val))
		for k := range val {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := val[k]
			if s, isString := child.(string); isString {
				if codexMessageFields[strings.ToLower(k)] {
					if found, ok := aiutil.ExtractJSONObject(s); ok {
						return found, true
					}
				}
				continue
			}
			if found, ok := searchJSONValue(child); ok {
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
// Model and Effort are appended only when set (same
// empty-means-CLI-default contract as NewClaude). Codex has no
// `--effort` flag: reasoning effort is a config key, overridden per
// invocation with `-c`. The value is emitted as a quoted TOML string
// because `-c` parses its value as TOML and only falls back to a raw
// literal when that parse fails — relying on the fallback would be
// depending on an error path.
func NewCodex(cfg config.CodexCLI) ai.StructuredGenerator {
	args := []string{"exec", "--json"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "-c", fmt.Sprintf("model_reasoning_effort=%q", cfg.Effort))
	}
	return &generator{
		bin:        cfg.Bin,
		args:       args,
		provider:   codexProvider,
		extract:    extractCodexTrailingLine,
		modelLabel: cfg.Model,
	}
}
