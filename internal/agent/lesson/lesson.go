// Package lesson implements the human-tutor lesson guide generator
// (PRD §18, §60): it turns a learner's recent priorities, dormant
// vocabulary, recent writing corrections, and learner-model
// observations into one schema-validated lesson_plan.v1 document a
// human tutor can read before a lesson.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters. It is a pure generator; the tutor
// lesson pipeline (application/lessons) owns gathering context from
// storage, authorization, persistence, and events. Generate returns the
// plan as verbatim, schema-validated JSON bytes — not unmarshaled into
// a Go struct — since nothing in this package or its caller needs to
// inspect individual fields; the raw JSON is exactly what gets
// persisted (storage.Lesson.Plan) and rendered by the detail template.
package lesson

import (
	"context"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName    = "lesson.generate"
	promptVersion = "v1"
	schemaName    = "lesson_plan.v1"
	agentName     = "lesson"
	// maxTokens is larger than anki's (512) or drill's: a lesson guide
	// has ten array sections (PRD §18), each up to 8 items, versus a
	// single flashcard's front/back/notes.
	maxTokens = 2048
)

// Agent generates one tutor lesson guide per call by asking gen for a
// schema-validated lesson_plan.v1 response. It holds no state beyond
// the generator: every call is a single, independent request.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a lesson agent backed by gen (fakeai for tests/offline
// dev, the Anthropic adapter in production, always through the
// observability decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// GenerateInput is everything Generate needs to render the
// lesson.generate prompt and scope the AI request. Every slice is
// already formatted as tutor-readable lines by the caller
// (application/lessons.Service) — this package does no formatting of
// its own beyond rendering the template, matching internal/agent/anki's
// "caller decides what the strings say" split. Any or all slices may be
// empty (a brand-new identity with no history yet); the user template
// guards each section with {{if}}.
type GenerateInput struct {
	Identity              learner.IdentityID
	Priorities            []string
	ActivationExpressions []string
	RecentCorrections     []string
	ObservationSummaries  []string
}

// promptData mirrors exactly what templates/lesson.generate.v1.*.md
// range/index over.
type promptData struct {
	Priorities            []string
	ActivationExpressions []string
	RecentCorrections     []string
	ObservationSummaries  []string
}

// Generate renders the lesson.generate prompt for in, asks gen for a
// lesson_plan.v1-shaped response (Rule 4: schema validation with one
// constrained-repair attempt and one full retry before failing, via
// aiutil.ValidateWithRepairAndRetry — the same bounded self-healing
// contract internal/agent/teacher, internal/agent/drill, and
// internal/agent/anki use), then returns the validated JSON verbatim.
func (a *Agent) Generate(ctx context.Context, in GenerateInput) (planJSON []byte, resp ai.StructuredResponse, err error) {
	data := promptData{
		Priorities:            in.Priorities,
		ActivationExpressions: in.ActivationExpressions,
		RecentCorrections:     in.RecentCorrections,
		ObservationSummaries:  in.ObservationSummaries,
	}
	rendered, err := prompts.Render(promptName, promptVersion, data)
	if err != nil {
		return nil, ai.StructuredResponse{}, fmt.Errorf("lesson: render prompt: %w", err)
	}

	schema, err := schemas.Get(schemaName)
	if err != nil {
		return nil, ai.StructuredResponse{}, fmt.Errorf("lesson: get schema: %w", err)
	}

	req := ai.StructuredRequest{
		PromptName:    promptName,
		PromptVersion: promptVersion,
		System:        rendered.System,
		User:          rendered.User,
		SchemaName:    schemaName,
		Schema:        schema,
		MaxTokens:     maxTokens,
		IdentityID:    in.Identity,
		Agent:         agentName,
	}

	resp, err = a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return nil, resp, fmt.Errorf("lesson: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, schemaName, req, resp)
	if err != nil {
		return nil, resp, err
	}

	return resp.JSON, resp, nil
}
