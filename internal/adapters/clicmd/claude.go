package clicmd

import (
	"encoding/json"
	"fmt"

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
type claudeEnvelope struct {
	Result string `json:"result"`
}

// extractClaudeResult unwraps claude -p --output-format json's
// envelope, returning .result's text for the shared balanced-JSON
// extraction step to search.
func extractClaudeResult(stdout []byte) ([]byte, error) {
	var env claudeEnvelope
	if err := json.Unmarshal(stdout, &env); err != nil {
		return nil, fmt.Errorf("decode claude -p --output-format json envelope: %w", err)
	}
	return []byte(env.Result), nil
}

// NewClaude returns an ai.StructuredGenerator that shells out to the
// Claude Code CLI (`claude -p --output-format json`, prompt on stdin)
// as a host-mode AI fallback (PRD §23). See the package doc comment
// for the host-mode-only caveat: cfg.Bin not being installed is not a
// construction-time error, only a per-call one.
func NewClaude(cfg config.ClaudeCLI) ai.StructuredGenerator {
	return &generator{
		bin:      cfg.Bin,
		args:     []string{"-p", "--output-format", "json"},
		provider: claudeProvider,
		extract:  extractClaudeResult,
	}
}
