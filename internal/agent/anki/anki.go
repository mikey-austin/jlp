// Package anki implements the Anki Card Generator agent (PRD §19): it
// turns one writing correction's source/corrected text into a single
// schema-validated Anki flashcard (front/back/notes) by rendering the
// anki.generate prompt and calling an ai.StructuredGenerator.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters. It is a pure generator; the review
// queue (application/anki) owns authorization, persistence, and
// events. Cards have no dedicated domain type (see
// storage.AnkiCard's doc comment: "cards are presentation data") —
// this agent returns front/back/notes as plain strings rather than
// reaching into storage or application types to avoid an import cycle.
package anki

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName    = "anki.generate"
	promptVersion = "v1"
	schemaName    = "anki_card.v1"
	agentName     = "anki"
	maxTokens     = 512
)

// Agent generates one Anki flashcard per call by asking gen for a
// schema-validated anki_card.v1 response. It holds no state beyond the
// generator: every call is a single, independent request.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns an Anki agent backed by gen (fakeai for tests/offline
// dev, the Anthropic adapter in production, always through the
// observability decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// GenerateInput is everything Generate needs to render the
// anki.generate prompt and scope the AI request. SourceType is a
// free-form label ("correction" today; PRD §19 lists other future
// sources such as recurring grammar problems or vocabulary) carried
// straight into the prompt, not validated here — application/anki.
// Service decides what counts as a valid source.
type GenerateInput struct {
	Identity                     learner.IdentityID
	SourceType                   string
	SourceText, CorrectedText    string
	ExplanationJA, ExplanationEN string
}

// promptData mirrors exactly what templates/anki.generate.v1.*.md
// range/index over.
type promptData struct {
	SourceType    string
	SourceText    string
	CorrectedText string
	ExplanationJA string
	ExplanationEN string
}

// cardDTO mirrors schemas/defs/anki_card.v1.json field-for-field, so
// unmarshaling a schema-valid response can never silently drop or
// misname a field.
type cardDTO struct {
	Front string `json:"front"`
	Back  string `json:"back"`
	Notes string `json:"notes,omitempty"`
}

// Generate renders the anki.generate prompt for in, asks gen for an
// anki_card.v1-shaped response (Rule 4: schema validation with one
// constrained-repair attempt and one full retry before failing, via
// aiutil.ValidateWithRepairAndRetry — the same bounded self-healing
// contract internal/agent/teacher and internal/agent/drill use), then
// returns the card's three fields directly.
func (a *Agent) Generate(ctx context.Context, in GenerateInput) (front, back, notes string, resp ai.StructuredResponse, err error) {
	data := promptData{
		SourceType:    in.SourceType,
		SourceText:    in.SourceText,
		CorrectedText: in.CorrectedText,
		ExplanationJA: in.ExplanationJA,
		ExplanationEN: in.ExplanationEN,
	}
	rendered, err := prompts.Render(promptName, promptVersion, data)
	if err != nil {
		return "", "", "", ai.StructuredResponse{}, fmt.Errorf("anki: render prompt: %w", err)
	}

	schema, err := schemas.Get(schemaName)
	if err != nil {
		return "", "", "", ai.StructuredResponse{}, fmt.Errorf("anki: get schema: %w", err)
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
		return "", "", "", resp, fmt.Errorf("anki: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, schemaName, req, resp)
	if err != nil {
		return "", "", "", resp, err
	}

	var dto cardDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return "", "", "", resp, fmt.Errorf("anki: unmarshal card: %w", err)
	}

	return dto.Front, dto.Back, dto.Notes, resp, nil
}
