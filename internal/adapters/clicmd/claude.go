package clicmd

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

const claudeProvider = "claudecli"

// claudeEnvelope mirrors `claude -p --output-format json`'s stdout: a
// JSON envelope wrapping the model's actual answer text in .result.
// This is the Claude Code CLI's own output-format contract, unrelated
// to req.Schema — the structured JSON we actually asked for lives
// INSIDE Result as plain text, pulled out by the shared balanced-JSON
// step in clicmd.go's GenerateStructured after this envelope is
// unwrapped.
// Usage/ModelUsage/IsError are all confirmed present in a live
// transcript (Claude Code CLI 2.1.232): usage carries real token
// counts, modelUsage is keyed by the model that actually answered
// (e.g. "claude-sonnet-5"), and is_error reports a failed turn that
// still exited 0.
type claudeEnvelope struct {
	Result  string `json:"result"`
	IsError bool   `json:"is_error"`
	Subtype string `json:"subtype"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
		// Prompt caching means most input tokens arrive under one of
		// these rather than input_tokens, which counted just 4 of 74420
		// in the transcript this was written against. Summing all three
		// is what makes the dashboard's input count meaningful.
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	} `json:"usage"`
	ModelUsage map[string]json.RawMessage `json:"modelUsage"`
}

// extractClaudeResult unwraps claude -p --output-format json's
// envelope, returning .result's text for the shared balanced-JSON
// extraction step to search, along with the usage and model the CLI
// reported about its own run.
func extractClaudeResult(stdout []byte) (cliOutput, error) {
	var env claudeEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return cliOutput{}, fmt.Errorf("decode claude -p --output-format json envelope: %w", err)
	}
	// A failed turn still exits 0 with is_error set, so exit status
	// alone would let an error message through to the balanced-JSON
	// step and surface as a confusing "no JSON object found".
	if env.IsError {
		return cliOutput{}, fmt.Errorf("claude reported an error turn (subtype %q): %s",
			env.Subtype, strings.TrimSpace(env.Result))
	}
	out := cliOutput{
		Text: []byte(env.Result),
		InputTokens: env.Usage.InputTokens +
			env.Usage.CacheCreationInputTokens + env.Usage.CacheReadInputTokens,
		OutputTokens: env.Usage.OutputTokens,
	}
	// modelUsage is keyed by model name. Exactly one key is the normal
	// case; more than one means the CLI fell back mid-turn, and naming
	// any single one of them would be a guess, so leave it to the
	// configured/placeholder value instead.
	if len(env.ModelUsage) == 1 {
		for name := range env.ModelUsage {
			out.Model = name
		}
	}
	return out, nil
}

// NewClaude returns an ai.StructuredGenerator that shells out to the
// Claude Code CLI (`claude -p --output-format json`, prompt on stdin)
// as a host-mode AI fallback (PRD §23). See the package doc comment
// for the host-mode-only caveat: cfg.Bin not being installed is not a
// construction-time error, only a per-call one.
// Model and Effort are appended only when set, so an operator who has
// already configured the CLI to their taste keeps that configuration —
// an unset field means "whatever the tool would do on its own", never
// a value this adapter invented.
func NewClaude(cfg config.ClaudeCLI) ai.StructuredGenerator {
	args := []string{"-p", "--output-format", "json"}
	if cfg.Model != "" {
		args = append(args, "--model", cfg.Model)
	}
	if cfg.Effort != "" {
		args = append(args, "--effort", cfg.Effort)
	}
	return &generator{
		bin:      cfg.Bin,
		args:     args,
		provider: claudeProvider,
		extract:  extractClaudeResult,
		// Unlike Anthropic's API adapter, the CLI never names the model
		// that answered — but when the operator PINNED one, that pin is
		// what ran, so report it instead of the "cli" placeholder.
		modelLabel: cfg.Model,
	}
}
