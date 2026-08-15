package clicmd_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/clicmd"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// writeStub writes an executable shell script to a fresh t.TempDir()
// and returns its absolute path. Tests point cfg.Bin directly at this
// path rather than relying on a PATH search — the host running these
// tests may have a REAL `claude`/`codex` CLI installed (this repo is
// itself developed with Claude Code), so resolving through PATH would
// risk accidentally invoking a live tool instead of the stub.
func writeStub(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write stub %s: %v", name, err)
	}
	return path
}

// claudeStub is the task brief's exact claude stub: it discards stdin,
// then answers the `claude -p --output-format json` envelope shape —
// a JSON object whose .result is itself a STRING containing the
// structured JSON the caller actually asked for.
const claudeStub = `#!/bin/sh
cat >/dev/null
echo '{"result":"{\"corrections\":[]}"}'
`

// codexStub mirrors `codex exec --json`'s shape: free-form progress
// text on earlier lines, with the actual answer as a trailing JSON
// line — nothing before it is valid JSON on its own, so extraction
// must anchor on the LAST line, not the first.
const codexStub = `#!/bin/sh
cat >/dev/null
printf 'thinking...\nrunning a tool...\n{"exercises":[]}\n'
`

func testRequest() ai.StructuredRequest {
	return ai.StructuredRequest{
		System: "SYSTEM PROMPT",
		User:   "USER PROMPT",
		Schema: json.RawMessage(`{"type":"object","properties":{}}`),
	}
}

func TestNewClaudeExtractsResultFromJSONEnvelope(t *testing.T) {
	bin := writeStub(t, "claude", claudeStub)
	gen := clicmd.NewClaude(config.ClaudeCLI{Bin: bin})

	resp, err := gen.GenerateStructured(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if string(resp.JSON) != `{"corrections":[]}` {
		t.Errorf("JSON = %s, want {\"corrections\":[]}", resp.JSON)
	}
	if resp.Provider != "claudecli" {
		t.Errorf("Provider = %q, want claudecli", resp.Provider)
	}
	if resp.Model != "cli" {
		t.Errorf("Model = %q, want cli", resp.Model)
	}
	if resp.InputTokens != 0 || resp.OutputTokens != 0 {
		t.Errorf("tokens = %d/%d, want 0/0 (CLI adapters never report token counts)", resp.InputTokens, resp.OutputTokens)
	}
}

func TestNewCodexExtractsTrailingJSONLine(t *testing.T) {
	bin := writeStub(t, "codex", codexStub)
	gen := clicmd.NewCodex(config.CodexCLI{Bin: bin})

	resp, err := gen.GenerateStructured(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	if string(resp.JSON) != `{"exercises":[]}` {
		t.Errorf("JSON = %s, want {\"exercises\":[]}", resp.JSON)
	}
	if resp.Provider != "codexcli" {
		t.Errorf("Provider = %q, want codexcli", resp.Provider)
	}
	if resp.Model != "cli" {
		t.Errorf("Model = %q, want cli", resp.Model)
	}
}

// TestNewCodexUnwrapsAnswerFromEventEnvelope pins the fix for a code
// review finding: `codex exec --help` documents --json as "Print
// events to stdout as JSONL", not necessarily the bare answer text on
// its own trailing line the way the task brief's simplified framing
// assumes. A real trailing line can be a protocol event object
// wrapping the model's answer in one of its own string fields (field
// name unconfirmed/version-dependent — this fixture uses a plausible
// `item.text` shape, but the extraction logic must not hardcode that
// exact name, only search generically). Without unwrapping, the
// balanced-JSON step would find the OUTER envelope object instead of
// the nested answer, and every real codexcli call would fail schema
// validation one layer up in aiutil.ValidateWithRepairAndRetry.
func TestNewCodexUnwrapsAnswerFromEventEnvelope(t *testing.T) {
	script := `#!/bin/sh
cat >/dev/null
printf '{"type":"turn.started"}\n{"type":"item.completed","item":{"type":"agent_message","text":"{\"exercises\":[{\"prompt\":\"x\"}]}"}}\n'
`
	bin := writeStub(t, "codex", script)
	gen := clicmd.NewCodex(config.CodexCLI{Bin: bin})

	resp, err := gen.GenerateStructured(context.Background(), testRequest())
	if err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}
	want := `{"exercises":[{"prompt":"x"}]}`
	if string(resp.JSON) != want {
		t.Errorf("JSON = %s, want %s (unwrapped from the item.text envelope field, not the outer event object)", resp.JSON, want)
	}
}

// TestPromptOnStdinIsSystemUserAndSchema pins the exact prompt-assembly
// contract from the task brief: System + "\n\n" + User + "\n\nRespond
// with ONLY a JSON object matching this schema:\n" + Schema, written
// to the CLI's stdin verbatim. The stub captures whatever it received
// on stdin to a file so the test can inspect it after the call.
func TestPromptOnStdinIsSystemUserAndSchema(t *testing.T) {
	dir := t.TempDir()
	captured := filepath.Join(dir, "captured-stdin.txt")
	script := "#!/bin/sh\ncat > " + captured + "\necho '{\"result\":\"{}\"}'\n"
	bin := writeStub(t, "claude", script)

	gen := clicmd.NewClaude(config.ClaudeCLI{Bin: bin})
	req := ai.StructuredRequest{
		System: "SYS",
		User:   "USR",
		Schema: json.RawMessage(`{"a":1}`),
	}
	if _, err := gen.GenerateStructured(context.Background(), req); err != nil {
		t.Fatalf("GenerateStructured: %v", err)
	}

	got, err := os.ReadFile(captured)
	if err != nil {
		t.Fatalf("read captured stdin: %v", err)
	}
	want := "SYS\n\nUSR\n\nRespond with ONLY a JSON object matching this schema:\n{\"a\":1}"
	if string(got) != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
}

// TestGenerateStructuredTimesOutWhenCLIHangs pins the 120s exec-context
// timeout: context.WithTimeout wraps whatever ctx the caller passes,
// so a short caller-supplied deadline (1s here) still governs, not
// just the internal 120s ceiling. A stub that sleeps past the caller's
// deadline must fail with an error wrapping context.DeadlineExceeded.
func TestGenerateStructuredTimesOutWhenCLIHangs(t *testing.T) {
	script := "#!/bin/sh\ncat >/dev/null\nsleep 3\necho '{\"result\":\"{}\"}'\n"
	bin := writeStub(t, "claude", script)
	gen := clicmd.NewClaude(config.ClaudeCLI{Bin: bin})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	start := time.Now()
	_, err := gen.GenerateStructured(ctx, testRequest())
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want an error wrapping context.DeadlineExceeded", err)
	}
	if elapsed >= 3*time.Second {
		t.Errorf("elapsed = %v, want well under the stub's 3s sleep (the 1s ctx deadline should have killed it)", elapsed)
	}
}

// TestGenerateStructuredErrorsCleanlyWhenBinaryMissing pins the
// host-mode contract: NewClaude/NewCodex never error at construction
// (see the package doc comment), only per-call, once
// GenerateStructured actually tries to exec a binary that isn't
// installed — the expected in-container failure mode.
func TestGenerateStructuredErrorsCleanlyWhenBinaryMissing(t *testing.T) {
	gen := clicmd.NewClaude(config.ClaudeCLI{Bin: "jlp-test-definitely-not-a-real-cli-binary-zzz"})

	_, err := gen.GenerateStructured(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected an error for a missing binary, got nil")
	}
}

func TestNewCodexErrorsCleanlyWhenBinaryMissing(t *testing.T) {
	gen := clicmd.NewCodex(config.CodexCLI{Bin: "jlp-test-definitely-not-a-real-cli-binary-zzz"})

	_, err := gen.GenerateStructured(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected an error for a missing binary, got nil")
	}
}

// TestNewCodexTimesOutWhenCLIHangs is TestGenerateStructuredTimesOutWhenCLIHangs's
// codex-side twin — the shared exec core in clicmd.go is what actually
// enforces the timeout, but both constructors must wire into it.
func TestNewCodexTimesOutWhenCLIHangs(t *testing.T) {
	script := "#!/bin/sh\ncat >/dev/null\nsleep 3\nprintf '{}\\n'\n"
	bin := writeStub(t, "codex", script)
	gen := clicmd.NewCodex(config.CodexCLI{Bin: bin})

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()

	_, err := gen.GenerateStructured(ctx, testRequest())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want an error wrapping context.DeadlineExceeded", err)
	}
}

// TestGenerateStructuredErrorsWhenNoJSONObjectInOutput pins the "AI
// refused / answered in prose only" failure mode: the shared
// balanced-JSON step (aiutil.ExtractJSONObject) finds nothing, and
// that must surface as an error rather than an empty/zero-value
// success response — same contract aiutil.ValidateWithRepairAndRetry
// relies on for every OTHER provider (a refusal falls through to a
// retry, never gets treated as a valid empty answer).
func TestGenerateStructuredErrorsWhenNoJSONObjectInOutput(t *testing.T) {
	script := `#!/bin/sh
cat >/dev/null
echo '{"result":"I cannot help with that."}'
`
	bin := writeStub(t, "claude", script)
	gen := clicmd.NewClaude(config.ClaudeCLI{Bin: bin})

	_, err := gen.GenerateStructured(context.Background(), testRequest())
	if err == nil {
		t.Fatal("expected an error when the CLI's answer has no JSON object in it, got nil")
	}
}
