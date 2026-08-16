// Package ai defines the port through which application code asks a
// language model for structured, schema-validated output. Every
// capability (teacher feedback today, more later) goes through
// StructuredGenerator so it can be satisfied by a real provider
// (Anthropic, Task 11), a deterministic fake (fakeai, for tests and
// offline dev), or an observability-wrapping decorator — all
// interchangeably, per hexagonal architecture.
package ai

import (
	"context"
	"encoding/json"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// StructuredRequest asks a generator to produce JSON conforming to
// Schema. System and User are already-rendered prompt text (see
// package prompts); PromptName/PromptVersion identify which template
// produced them, for observability and traceability.
type StructuredRequest struct {
	PromptName    string // "teacher.feedback"
	PromptVersion string // "v1"
	System, User  string // fully rendered prompt text
	SchemaName    string // "correction_result.v1"
	Schema        json.RawMessage
	MaxTokens     int
	IdentityID    learner.IdentityID
	SessionID     *session.ID
	Agent         string // "teacher"
	// ProviderOverride, when non-empty, asks airouter to dispatch this
	// request to EXACTLY the named provider — no PromptName-based chain
	// lookup, no fallback to a next chain member on failure (Phase 4
	// Task W's workspace adapter-override dropdown: "an explicit
	// override does not fall back," see airouter's own doc comment).
	// Empty (the default, and every caller before this field existed)
	// leaves routing exactly as before: airouter.router picks a chain by
	// PromptName and falls through it on error. Only airouter's router
	// reads this field — every leaf ai.StructuredGenerator (fakeai,
	// anthropic, ollama, clicmd, agycli) IS already a specific provider,
	// so the field is meaningless to them and they ignore it.
	ProviderOverride string
}

// StructuredResponse is a generator's answer: JSON validated against
// the request's Schema, plus provenance and cost/latency metadata.
// RequestID is empty until an observability.NewAIObserver has stamped
// it with the corresponding ai_requests.id.
type StructuredResponse struct {
	RequestID    string // set by the observability decorator == ai_requests.id
	JSON         json.RawMessage
	Provider     string
	Model        string
	InputTokens  int
	OutputTokens int
	Latency      time.Duration
}

// StructuredGenerator produces schema-validated JSON from a prompt.
// Implementations: adapters/fakeai (deterministic, offline),
// adapters/anthropic (Task 11), observability.NewAIObserver (wraps
// either, recording every call).
type StructuredGenerator interface {
	GenerateStructured(ctx context.Context, req StructuredRequest) (StructuredResponse, error)
}
