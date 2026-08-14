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
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName    = "teacher.feedback"
	promptVersion = "v2"
	schemaName    = "correction_result.v1"
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
	// ConceptCandidates is the tagging vocabulary the teacher.feedback.v2
	// prompt offers the model: one "slug — name" line per grammar
	// concept in the catalog (see application/feedback.Service, which
	// builds this from storage.GrammarRepository.ListConcepts). The
	// model may only tag a correction's concepts from this list — see
	// the v2 system template's "chosen ONLY from the provided candidate
	// list" instruction.
	ConceptCandidates []string
}

// promptData mirrors exactly what templates/teacher.feedback.v2.*.md
// range/index over.
type promptData struct {
	TeacherMode         string
	Strictness          string
	ExplanationLanguage string
	Purpose             string
	Audience            string
	Register            string
	Context             string
	Selection           string
	RecentErrors        []string
	ConceptCandidates   []string
}

// The following DTOs mirror schemas/defs/correction_result.v1.json
// field-for-field, so unmarshaling a schema-valid response can never
// silently drop or misname a field.
type explanationDTO struct {
	JA string `json:"ja"`
	EN string `json:"en"`
}

type correctionDTO struct {
	Original    string         `json:"original"`
	Replacement string         `json:"replacement"`
	Type        string         `json:"type"`
	Severity    string         `json:"severity"`
	Explanation explanationDTO `json:"explanation"`
	Concepts    []string       `json:"concepts,omitempty"`
}

type correctionResultDTO struct {
	Corrections []correctionDTO `json:"corrections"`
}

// ReviewWriting renders the teacher.feedback prompt for in, asks gen
// for a correction_result.v1-shaped response (Rule 4: schema
// validation with one constrained-repair attempt and one full retry
// before failing), then maps the result onto correction.NewResult so
// the caller gets both the domain Result and the raw AI response (the
// latter carries RequestID/provenance the feedback pipeline persists).
func (a *Agent) ReviewWriting(ctx context.Context, in ReviewInput) (correction.Result, ai.StructuredResponse, error) {
	data := promptData{
		TeacherMode:         in.Session.Profile.TeacherMode,
		Strictness:          in.Session.Profile.Strictness,
		ExplanationLanguage: in.Session.Profile.ExplanationLanguage,
		Purpose:             in.Session.Purpose,
		Audience:            in.Session.Profile.Audience,
		Register:            in.Session.Profile.Register,
		Context:             in.Context,
		Selection:           in.Selection,
		RecentErrors:        in.RecentErrors,
		ConceptCandidates:   in.ConceptCandidates,
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

	resp, err = a.validateWithRepairAndRetry(ctx, req, resp)
	if err != nil {
		return correction.Result{}, resp, err
	}

	var dto correctionResultDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return correction.Result{}, resp, fmt.Errorf("teacher: unmarshal corrections: %w", err)
	}

	cs := make([]correction.Correction, 0, len(dto.Corrections))
	for _, c := range dto.Corrections {
		cs = append(cs, correction.Correction{
			Original:    c.Original,
			Replacement: c.Replacement,
			Type:        correction.Type(c.Type),
			Severity:    correction.Severity(c.Severity),
			Explanation: correction.Explanation{JA: c.Explanation.JA, EN: c.Explanation.EN},
			Concepts:    c.Concepts,
		})
	}

	result, err := correction.NewResult(in.Selection, cs)
	if err != nil {
		return correction.Result{}, resp, fmt.Errorf("teacher: apply corrections: %w", err)
	}
	return result, resp, nil
}

// validateWithRepairAndRetry implements Rule 4: schema validation with
// bounded self-healing. It tries, in order: the response as given; one
// constrained repair (extract the first balanced {...} block from the
// raw text, e.g. to strip a markdown code fence, and re-validate that);
// one full retry call to gen. If none validates, it returns a wrapped
// error rather than looping further — an AI response that still won't
// validate after both is a hard failure.
func (a *Agent) validateWithRepairAndRetry(ctx context.Context, req ai.StructuredRequest, resp ai.StructuredResponse) (ai.StructuredResponse, error) {
	if err := schemas.Validate(schemaName, resp.JSON); err == nil {
		return resp, nil
	}

	if repaired, ok := extractBalancedObject(resp.JSON); ok {
		if err := schemas.Validate(schemaName, repaired); err == nil {
			resp.JSON = repaired
			return resp, nil
		}
	}

	retried, err := a.gen.GenerateStructured(ctx, req)
	if err != nil {
		return resp, fmt.Errorf("teacher: retry after invalid response: %w", err)
	}
	if err := schemas.Validate(schemaName, retried.JSON); err != nil {
		return retried, fmt.Errorf("teacher: response failed schema validation after repair and retry: %w", err)
	}
	return retried, nil
}

// extractBalancedObject finds the first '{' in raw and returns the
// substring through its matching '}', tracking brace depth and JSON
// string literals (so braces inside quoted strings don't throw off the
// count). It reports ok=false if raw contains no balanced {...} block —
// e.g. a refusal message with no JSON in it at all, which repair can't
// fix and the caller must fall back to a retry for.
func extractBalancedObject(raw []byte) (json.RawMessage, bool) {
	start := bytes.IndexByte(raw, '{')
	if start == -1 {
		return nil, false
	}

	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(raw); i++ {
		c := raw[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return json.RawMessage(raw[start : i+1]), true
			}
		}
	}
	return nil, false
}
