// Package summary implements the progress-analyst weekly email digest
// generator (PRD §21, §65): it turns a learner's aggregate statistics,
// scored priorities, and recently-active vocabulary into a single
// schema-validated weekly_summary.v1 document, then composes that
// document into the actual plain-text email subject/body a human
// reads — Generate returns the two strings ready to hand straight to
// a notifications.Notifier, not the raw JSON (contrast
// internal/agent/lesson, which returns raw JSON for a template to
// render, and internal/agent/anki, which returns three unmarshaled
// scalar fields directly from the JSON with no further composition).
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters (see .golangci.yml's agents-no-repos
// rule, which denies internal/ports/storage here same as every other
// agent package). storage.Statistics is therefore never referenced in
// this package: application/summary.Service pre-formats it into
// StatsSummary ([]string) before calling Generate, the same "caller
// decides what the strings say" convention internal/agent/lesson's
// GenerateInput doc comment describes for its own Priorities/
// ActivationExpressions/RecentCorrections/ObservationSummaries fields.
package summary

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName    = "summary.generate"
	promptVersion = "v1"
	schemaName    = "weekly_summary.v1"
	agentName     = "summary"
	// maxTokens covers five 1-8-item arrays plus a subject and an
	// optional challenge sentence — comparable to lesson's ten sections
	// (2048) but with fewer, shorter fields, so a smaller budget suffices.
	maxTokens = 1536
)

// Agent generates one weekly summary email per call by asking gen for
// a schema-validated weekly_summary.v1 response, then composing it into
// a ready-to-send subject/body pair. It holds no state beyond the
// generator: every call is a single, independent request.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a summary agent backed by gen (fakeai for tests/offline
// dev, the Anthropic adapter in production, always through the
// observability decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// SummaryInput is everything Generate needs to render the
// summary.generate prompt and scope the AI request. StatsSummary,
// Priorities, and NewExpressions are already formatted as
// learner-readable lines by the caller (application/summary.Service) —
// this package does no formatting of its own beyond rendering the
// template and composing the final email body from the model's
// response. Any or all slices may be empty (a brand-new identity with
// no history yet); the user template guards each section with {{if}}.
type SummaryInput struct {
	Identity       learner.IdentityID
	StatsSummary   []string
	Priorities     []string
	NewExpressions []string
}

// promptData mirrors exactly what templates/summary.generate.v1.*.md
// range/index over.
type promptData struct {
	StatsSummary   []string
	Priorities     []string
	NewExpressions []string
}

// weeklySummaryDTO mirrors schemas/defs/weekly_summary.v1.json
// field-for-field, so unmarshaling a schema-valid response can never
// silently drop or misname a field.
type weeklySummaryDTO struct {
	Subject              string   `json:"subject"`
	Accomplishments      []string `json:"accomplishments"`
	Improvements         []string `json:"improvements"`
	PersistentWeaknesses []string `json:"persistent_weaknesses"`
	NewExpressions       []string `json:"new_expressions"`
	RecommendedFocus     []string `json:"recommended_focus"`
	Challenge            string   `json:"challenge,omitempty"`
}

// Generate renders the summary.generate prompt for in, asks gen for a
// weekly_summary.v1-shaped response (Rule 4: schema validation with one
// constrained-repair attempt and one full retry before failing, via
// aiutil.ValidateWithRepairAndRetry — the same bounded self-healing
// contract internal/agent/teacher, internal/agent/drill,
// internal/agent/anki, and internal/agent/lesson all use), then
// composes the validated JSON into a plain-text email: subject verbatim
// from the JSON, textBody assembled Go-side rather than left to the
// model — see composeBody — so the email's structure is deterministic
// and independent of exactly how the model happened to phrase its
// response.
func (a *Agent) Generate(ctx context.Context, in SummaryInput) (subject, textBody string, resp ai.StructuredResponse, err error) {
	data := promptData{
		StatsSummary:   in.StatsSummary,
		Priorities:     in.Priorities,
		NewExpressions: in.NewExpressions,
	}
	rendered, err := prompts.Render(promptName, promptVersion, data)
	if err != nil {
		return "", "", ai.StructuredResponse{}, fmt.Errorf("summary: render prompt: %w", err)
	}

	schema, err := schemas.Get(schemaName)
	if err != nil {
		return "", "", ai.StructuredResponse{}, fmt.Errorf("summary: get schema: %w", err)
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
		return "", "", resp, fmt.Errorf("summary: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, schemaName, req, resp)
	if err != nil {
		return "", "", resp, err
	}

	var dto weeklySummaryDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return "", "", resp, fmt.Errorf("summary: unmarshal weekly summary: %w", err)
	}

	return dto.Subject, composeBody(dto), resp, nil
}

// composeBody assembles the plain-text email body from dto — Go-side,
// not model-side (PRD §21/§56): every section is a fixed, predictable
// heading followed by "- "-bulleted lines, so the email's overall
// structure never depends on how the model happens to phrase things,
// only the content within each bullet does. challenge is only included
// when the model actually filled it in (PRD §21's "optional next
// week's challenge").
func composeBody(dto weeklySummaryDTO) string {
	var b strings.Builder
	writeSection(&b, "What you accomplished this week", dto.Accomplishments)
	writeSection(&b, "Biggest improvements", dto.Improvements)
	writeSection(&b, "Areas to keep working on", dto.PersistentWeaknesses)
	writeSection(&b, "New expressions worth reviewing", dto.NewExpressions)
	writeSection(&b, "Recommended focus for next week", dto.RecommendedFocus)
	if dto.Challenge != "" {
		b.WriteString("A challenge for next week (totally optional):\n")
		b.WriteString(dto.Challenge)
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// writeSection appends one heading + bulleted-lines block to b, or
// nothing at all when items is empty — only new_expressions can
// legitimately be empty (see the schema's new_expressions minItems:0),
// but this stays generic rather than special-casing that one field.
func writeSection(b *strings.Builder, heading string, items []string) {
	if len(items) == 0 {
		return
	}
	b.WriteString(heading)
	b.WriteString(":\n")
	for _, item := range items {
		b.WriteString("- ")
		b.WriteString(item)
		b.WriteString("\n")
	}
	b.WriteString("\n")
}
