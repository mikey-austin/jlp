// Package reading implements the 読解 study-edition generator: it turns
// one Japanese article into a schema-validated study_edition.v1 lesson
// (vocabulary, grammar, 精読 of the hardest sentences, review
// questions) in a single structured-generation call.
//
// One call rather than one per section on purpose: the sections are
// about the same text and should agree with each other (a grammar note
// quoting a sentence the 精読 also breaks down), one call is one
// latency and one bill, and the whole lesson either validates or is
// retried together. Splitting it later is a new prompt version, not a
// new pipeline.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY. The pipeline (application/reading) owns persistence, retries
// and delivery; this is a pure generator.
package reading

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domain "github.com/mikeyaustin/jlp/internal/domain/reading"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	// PromptName is also the APP_AI_ROUTES key, e.g.
	// APP_AI_ROUTES=reading.analyse=gemini.
	PromptName    = "reading.analyse"
	PromptVersion = "v1"
	SchemaName    = "study_edition.v1"
	agentName     = "reader"
	// maxTokens is the largest of any agent's: up to 25 vocabulary
	// entries with two example sentences each, plus grammar, sentence
	// breakdowns and review questions, in two languages. 8k covers a
	// full lesson with room to spare; a model that stops short produces
	// truncated JSON, which fails validation and is retried rather than
	// shipped.
	maxTokens = 8192
)

// articleMarkers delimit the untrusted article in the user prompt. They
// are removed from the article text itself before rendering, so a page
// cannot close the block early and have its own text read as prompt.
var articleMarkers = strings.NewReplacer("<<<ARTICLE", "", "ARTICLE>>>", "")

// Agent generates one study edition per call.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a reading agent backed by gen (the observed, routed
// generator main.go builds — Gemini, Anthropic, Ollama or fake).
func New(gen ai.StructuredGenerator) *Agent { return &Agent{gen: gen} }

// Input is what Analyse needs: the article and who it is for.
type Input struct {
	Identity     learner.IdentityID
	Title        string
	Source       string
	Published    string // preformatted, e.g. "2026-09-29"; empty when unknown
	Text         string
	LearnerLevel string
}

type promptData struct {
	LearnerLevel, Title, Source, Published, Text string
}

// Analyse renders reading.analyse, asks gen for a study_edition.v1
// document (Rule 4: one constrained repair and one full retry via
// aiutil.ValidateWithRepairAndRetry), then decodes and normalises it
// (domain.Lesson.Normalise — the semantic checks the schema cannot
// express). It returns the lesson and the provider's response, whose
// RequestID links the edition to its ai_requests row on /ai.
func (a *Agent) Analyse(ctx context.Context, in Input) (domain.Lesson, ai.StructuredResponse, error) {
	level := strings.TrimSpace(in.LearnerLevel)
	if level == "" {
		level = "advanced (JLPT N2–N1)"
	}
	rendered, err := prompts.Render(PromptName, PromptVersion, promptData{
		LearnerLevel: level,
		Title:        articleMarkers.Replace(in.Title),
		Source:       articleMarkers.Replace(in.Source),
		Published:    in.Published,
		Text:         articleMarkers.Replace(in.Text),
	})
	if err != nil {
		return domain.Lesson{}, ai.StructuredResponse{}, fmt.Errorf("reading: render prompt: %w", err)
	}
	schema, err := schemas.Get(SchemaName)
	if err != nil {
		return domain.Lesson{}, ai.StructuredResponse{}, fmt.Errorf("reading: get schema: %w", err)
	}
	req := ai.StructuredRequest{
		PromptName:    PromptName,
		PromptVersion: PromptVersion,
		System:        rendered.System,
		User:          rendered.User,
		SchemaName:    SchemaName,
		Schema:        schema,
		MaxTokens:     maxTokens,
		IdentityID:    in.Identity,
		Agent:         agentName,
	}

	resp, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return domain.Lesson{}, resp, fmt.Errorf("reading: generate: %w", err)
	}
	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, SchemaName, req, resp)
	if err != nil {
		return domain.Lesson{}, resp, fmt.Errorf("reading: %w", err)
	}

	var lesson domain.Lesson
	if err := json.Unmarshal(resp.JSON, &lesson); err != nil {
		return domain.Lesson{}, resp, fmt.Errorf("reading: decode validated lesson: %w", err)
	}
	if err := lesson.Normalise(); err != nil {
		return domain.Lesson{}, resp, err
	}
	return lesson, resp, nil
}
