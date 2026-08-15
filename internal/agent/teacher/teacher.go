// Package teacher implements the Teacher agent: turning a learner's
// selected text into schema-validated corrections by rendering the
// teacher.feedback prompt and calling an ai.StructuredGenerator.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters. The agent is a pure reviewer; the
// feedback pipeline (application/feedback) owns authorization,
// persistence, and events.
package teacher

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName = "teacher.feedback"
	// promptVersion/schemaName are ALWAYS v3/v2, for every teacher mode
	// — not just "socratic" — per Phase 2 Task 8's controller
	// resolution: schema v2 is a strict superset of v1 (an optional
	// `hint` per correction), so a non-socratic response with no hints
	// still validates, and keeping one prompt/schema pair in use (rather
	// than branching per-mode) keeps prompt/schema comparisons
	// apples-to-apples across every session. v1 stays registered (and
	// fakeai still accepts it) only because it's still the direct
	// subject of package schemas/fakeai's own tests, not because
	// anything in this package still requests it.
	promptVersion = "v3"
	schemaName    = "correction_result.v2"
	agentName     = "teacher"
	maxTokens     = 2048
)

// Agent reviews learner writing by asking gen for schema-validated
// corrections. It holds no state beyond the generator: every review is
// a single, independent call.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a Teacher agent backed by gen (fakeai for tests/offline
// dev, the Anthropic adapter in production, always through the
// observability decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// ReviewInput is everything ReviewWriting needs to render the
// teacher.feedback prompt and scope the AI request.
type ReviewInput struct {
	Identity     learner.IdentityID
	Session      session.Session
	Selection    string
	Context      string   // surrounding document text
	RecentErrors []string // human-readable weakness summaries (empty in MVP; Phase 2 fills it)
	// ConceptCandidates is the tagging vocabulary the teacher.feedback.v3
	// prompt offers the model: one "slug — name" line per grammar
	// concept in the catalog (see application/feedback.Service, which
	// builds this from storage.GrammarRepository.ListConcepts). The
	// model may only tag a correction's concepts from this list — see
	// the v3 system template's "chosen ONLY from the provided candidate
	// list" instruction.
	ConceptCandidates []string
	// ExpressionsToEncourage is PRD §55/§17.5's vocabulary activator:
	// up to 5 "expression (reading) — meaning" lines for expressions the
	// learner knows but hasn't produced (see
	// application/planner.Planner.ActivationCandidates, which
	// application/feedback.Service formats this from). Unlike
	// RecentErrors/ConceptCandidates, this is advisory, not
	// instructional — the v3 user template asks the model to weave ONE
	// in only when it fits naturally, never to force it.
	ExpressionsToEncourage []string
}

// promptData mirrors exactly what templates/teacher.feedback.v3.*.md
// range/index over.
type promptData struct {
	TeacherMode            string
	Strictness             string
	ExplanationLanguage    string
	Purpose                string
	Audience               string
	Register               string
	Context                string
	Selection              string
	RecentErrors           []string
	ConceptCandidates      []string
	ExpressionsToEncourage []string
}

// The following DTOs mirror schemas/defs/correction_result.v2.json
// field-for-field, so unmarshaling a schema-valid response can never
// silently drop or misname a field.
type explanationDTO struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

type correctionDTO struct {
	Original    string          `json:"original"`
	Replacement string          `json:"replacement"`
	Type        string          `json:"type"`
	Severity    string          `json:"severity"`
	Explanation explanationDTO  `json:"explanation"`
	Concepts    []string        `json:"concepts,omitempty"`
	Hint        *explanationDTO `json:"hint,omitempty"`
}

type correctionResultDTO struct {
	Corrections []correctionDTO `json:"corrections"`
}

// ReviewWriting renders the teacher.feedback prompt for in, asks gen
// for a correction_result.v2-shaped response (Rule 4: schema
// validation with one constrained-repair attempt and one full retry
// before failing), then maps the result onto correction.NewResult so
// the caller gets both the domain Result and the raw AI response (the
// latter carries RequestID/provenance the feedback pipeline persists).
func (a *Agent) ReviewWriting(ctx context.Context, in ReviewInput) (correction.Result, ai.StructuredResponse, error) {
	data := promptData{
		TeacherMode:            in.Session.Profile.TeacherMode,
		Strictness:             in.Session.Profile.Strictness,
		ExplanationLanguage:    in.Session.Profile.ExplanationLanguage,
		Purpose:                in.Session.Purpose,
		Audience:               in.Session.Profile.Audience,
		Register:               in.Session.Profile.Register,
		Context:                in.Context,
		Selection:              in.Selection,
		RecentErrors:           in.RecentErrors,
		ConceptCandidates:      in.ConceptCandidates,
		ExpressionsToEncourage: in.ExpressionsToEncourage,
	}
	rendered, err := prompts.Render(promptName, promptVersion, data)
	if err != nil {
		return correction.Result{}, ai.StructuredResponse{}, fmt.Errorf("teacher: render prompt: %w", err)
	}

	schema, err := schemas.Get(schemaName)
	if err != nil {
		return correction.Result{}, ai.StructuredResponse{}, fmt.Errorf("teacher: get schema: %w", err)
	}

	sid := in.Session.ID
	req := ai.StructuredRequest{
		PromptName:    promptName,
		PromptVersion: promptVersion,
		System:        rendered.System,
		User:          rendered.User,
		SchemaName:    schemaName,
		Schema:        schema,
		MaxTokens:     maxTokens,
		IdentityID:    in.Identity,
		SessionID:     &sid,
		Agent:         agentName,
	}

	resp, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return correction.Result{}, resp, fmt.Errorf("teacher: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, schemaName, req, resp)
	if err != nil {
		return correction.Result{}, resp, err
	}

	var dto correctionResultDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return correction.Result{}, resp, fmt.Errorf("teacher: unmarshal corrections: %w", err)
	}

	cs := make([]correction.Correction, 0, len(dto.Corrections))
	for _, c := range dto.Corrections {
		cc := correction.Correction{
			Original:    c.Original,
			Replacement: c.Replacement,
			Type:        correction.Type(c.Type),
			Severity:    correction.Severity(c.Severity),
			Explanation: correction.Explanation{JA: c.Explanation.JA, EN: c.Explanation.EN},
			Concepts:    c.Concepts,
		}
		if c.Hint != nil {
			cc.Hint = correction.Explanation{JA: c.Hint.JA, EN: c.Hint.EN}
		}
		cs = append(cs, cc)
	}

	result, err := correction.NewResult(in.Selection, cs)
	if err != nil {
		return correction.Result{}, resp, fmt.Errorf("teacher: apply corrections: %w", err)
	}
	return result, resp, nil
}
