package clicmd

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
)

// The fixtures in testdata are REAL stdout, captured from
// `claude -p --output-format json --model sonnet --effort low` (Claude
// Code CLI 2.1.232) and `codex exec --json -c model_reasoning_effort=low`
// (Codex CLI 0.135.0), both fed this repo's actual teacher.feedback
// prompt and correction_result.v1 schema.
//
// Until these were captured, both extractors were written against
// guessed output shapes — codex.go said so in as many words ("no
// live-tested transcript to pin an exact schema against"). Speculative
// parsers age badly and fail in production, not in CI; these tests
// replace the guess with the real bytes, and will fail loudly if a
// future CLI version changes the contract.

func TestExtractClaudeResultAgainstALiveTranscript(t *testing.T) {
	stdout, err := os.ReadFile("testdata/claude_live.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	out, err := extractClaudeResult(stdout)
	if err != nil {
		t.Fatalf("extractClaudeResult: %v", err)
	}

	// The answer arrives inside a ```json fence, which is exactly what
	// the shared balanced-JSON step exists to see through.
	obj, ok := aiutil.ExtractJSONObject(string(out.Text))
	if !ok {
		t.Fatalf("no JSON object in extracted text: %.200s", out.Text)
	}
	var parsed struct {
		Corrections []struct {
			Original    string `json:"original"`
			Replacement string `json:"replacement"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(obj, &parsed); err != nil {
		t.Fatalf("unmarshal answer: %v", err)
	}
	if len(parsed.Corrections) == 0 {
		t.Fatal("answer carried no corrections")
	}
	if got := parsed.Corrections[0].Original; got != "面白いでした" {
		t.Errorf("first correction original = %q, want %q", got, "面白いでした")
	}

	// Usage and model: the values this exact transcript reported.
	// input = 4 + 37377 (cache creation) + 37039 (cache read).
	if want := 4 + 37377 + 37039; out.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d", out.InputTokens, want)
	}
	if want := 839; out.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d", out.OutputTokens, want)
	}
	if want := "claude-sonnet-5"; out.Model != want {
		t.Errorf("Model = %q, want %q (from modelUsage)", out.Model, want)
	}
}

// TestExtractClaudeResultRejectsAnErrorTurn: the CLI exits 0 on a failed
// turn and reports it in the envelope, so without this check the error
// text would flow into the balanced-JSON step and surface as a
// misleading "no JSON object found".
func TestExtractClaudeResultRejectsAnErrorTurn(t *testing.T) {
	stdout := []byte(`{"is_error":true,"subtype":"error_max_turns","result":"hit the turn limit"}`)
	if _, err := extractClaudeResult(stdout); err == nil {
		t.Fatal("extractClaudeResult accepted an is_error envelope, want error")
	}
}

func TestExtractCodexTrailingLineAgainstALiveTranscript(t *testing.T) {
	stdout, err := os.ReadFile("testdata/codex_live.jsonl")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	out, err := extractCodexTrailingLine(stdout)
	if err != nil {
		t.Fatalf("extractCodexTrailingLine: %v", err)
	}

	obj, ok := aiutil.ExtractJSONObject(string(out.Text))
	if !ok {
		t.Fatalf("no JSON object in extracted text: %.200s", out.Text)
	}
	var parsed struct {
		Corrections []struct {
			Original string `json:"original"`
		} `json:"corrections"`
	}
	if err := json.Unmarshal(obj, &parsed); err != nil {
		t.Fatalf("unmarshal answer: %v", err)
	}
	if len(parsed.Corrections) == 0 {
		t.Fatal("answer carried no corrections")
	}
	if got := parsed.Corrections[0].Original; got != "面白いでした" {
		t.Errorf("first correction original = %q, want %q", got, "面白いでした")
	}

	// The decisive detail: the answer is NOT on the trailing line —
	// turn.completed is. An extractor that trusted "last line = answer"
	// would fail this test, and usage would be lost.
	if want := 14168 + 2432; out.InputTokens != want {
		t.Errorf("InputTokens = %d, want %d", out.InputTokens, want)
	}
	if want := 231; out.OutputTokens != want {
		t.Errorf("OutputTokens = %d, want %d", out.OutputTokens, want)
	}
}
