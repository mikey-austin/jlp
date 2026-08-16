// Internal test (package clicmd, not clicmd_test): these assertions are
// about how each CLI's flags are ASSEMBLED, which is deliberately
// unexported — the exported surface is only ai.StructuredGenerator.
// Checking the argv the adapter would run is the only way to pin the
// per-tool spelling without actually invoking a paid CLI.
package clicmd

import (
	"slices"
	"testing"

	"github.com/mikeyaustin/jlp/internal/config"
)

// TestModelAndEffortReachTheCommandLine pins how each CLI's model and
// effort settings are spelled. The two tools disagree — Claude Code
// takes --effort, Codex has no such flag and wants a -c config
// override — so a shared "effort" concept must not be assumed to map to
// a shared flag. Unset fields must add nothing at all: an operator who
// has already configured their CLI keeps that configuration.
func TestModelAndEffortReachTheCommandLine(t *testing.T) {
	t.Run("claude", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cfg  config.ClaudeCLI
			want []string
		}{
			{"bare", config.ClaudeCLI{Bin: "claude"},
				[]string{"-p", "--output-format", "json"}},
			{"model only", config.ClaudeCLI{Bin: "claude", Model: "opus"},
				[]string{"-p", "--output-format", "json", "--model", "opus"}},
			{"model and effort", config.ClaudeCLI{Bin: "claude", Model: "opus", Effort: "xhigh"},
				[]string{"-p", "--output-format", "json", "--model", "opus", "--effort", "xhigh"}},
			{"effort only", config.ClaudeCLI{Bin: "claude", Effort: "low"},
				[]string{"-p", "--output-format", "json", "--effort", "low"}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := NewClaude(tc.cfg, nil).(*generator)
				got := g.buildArgs(g.baseModel, g.baseEffort)
				if !slices.Equal(got, tc.want) {
					t.Errorf("args = %q, want %q", got, tc.want)
				}
			})
		}
	})

	t.Run("codex", func(t *testing.T) {
		for _, tc := range []struct {
			name string
			cfg  config.CodexCLI
			want []string
		}{
			{"bare", config.CodexCLI{Bin: "codex"},
				[]string{"exec", "--json"}},
			{"model only", config.CodexCLI{Bin: "codex", Model: "gpt-5.5-codex"},
				[]string{"exec", "--json", "--model", "gpt-5.5-codex"}},
			// The effort value must be a QUOTED TOML string: `-c` parses
			// its value as TOML and only falls back to a raw literal when
			// that parse fails, so emitting it bare would be relying on an
			// error path to do the right thing.
			{"model and effort", config.CodexCLI{Bin: "codex", Model: "gpt-5.5-codex", Effort: "high"},
				[]string{"exec", "--json", "--model", "gpt-5.5-codex", "-c", `model_reasoning_effort="high"`}},
			{"effort only", config.CodexCLI{Bin: "codex", Effort: "minimal"},
				[]string{"exec", "--json", "-c", `model_reasoning_effort="minimal"`}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				g := NewCodex(tc.cfg, nil).(*generator)
				got := g.buildArgs(g.baseModel, g.baseEffort)
				if !slices.Equal(got, tc.want) {
					t.Errorf("args = %q, want %q", got, tc.want)
				}
			})
		}
	})
}

// TestReportedModelPrefersThePinnedModel: "cli" is honest only while
// nobody has pinned a model. Once --model is passed, that IS what ran,
// and ai_requests should record it rather than a placeholder that makes
// every CLI run look identical on the AI dashboard.
func TestReportedModelPrefersThePinnedModel(t *testing.T) {
	if got := NewClaude(config.ClaudeCLI{Bin: "claude"}, nil).(*generator).reportedModel(); got != "cli" {
		t.Errorf("unpinned model = %q, want %q", got, "cli")
	}
	if got := NewClaude(config.ClaudeCLI{Bin: "claude", Model: "opus"}, nil).(*generator).reportedModel(); got != "opus" {
		t.Errorf("pinned model = %q, want %q", got, "opus")
	}
	if got := NewCodex(config.CodexCLI{Bin: "codex", Model: "gpt-5.5-codex"}, nil).(*generator).reportedModel(); got != "gpt-5.5-codex" {
		t.Errorf("pinned model = %q, want %q", got, "gpt-5.5-codex")
	}
}
