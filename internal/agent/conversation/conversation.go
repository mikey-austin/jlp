// Package conversation implements the conversation tutor agent (Phase 4
// Task 6, PRD §17.4): turning one learner message — plus the
// conversation's history so far — into a schema-validated reply, an
// optional set of corrections on what the learner said, and an
// optional follow-up question, by rendering the conversation.turn
// prompt and calling an ai.StructuredGenerator.
//
// Rule 3: this package imports ports/ai + prompts + schemas + domain
// ONLY — no storage, no adapters. The agent has no notion of feedback
// timing, persistence, or events; application/conversation.Service owns
// all of that, exactly like application/feedback.Service wraps
// agent/teacher.
package conversation

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/aiutil"
	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/prompts"
	"github.com/mikeyaustin/jlp/internal/schemas"
)

const (
	promptName    = "conversation.turn"
	promptVersion = "v1"
	schemaName    = "conversation_turn.v1"
	agentName     = "conversation"
	maxTokens     = 1024
)

// Agent drives the conversation tutor's per-turn structured generation.
// It holds no conversation state itself — every Turn call is
// independent, given whatever History the caller supplies.
type Agent struct {
	gen ai.StructuredGenerator
}

// New returns a conversation tutor agent backed by gen (fakeai for
// tests/offline dev, the Anthropic adapter in production, always
// through the observability decorator).
func New(gen ai.StructuredGenerator) *Agent {
	return &Agent{gen: gen}
}

// HistoryTurn is one prior exchange in the conversation, rendered into
// the conversation.turn.v1 user prompt so the model has continuity —
// mirrors exactly the two fields the template's {{range .History}}
// loop reads.
type HistoryTurn struct {
	LearnerText string
	Reply       string
}

// TurnInput is everything Turn needs to render the conversation.turn
// prompt and scope the AI request.
type TurnInput struct {
	Identity learner.IdentityID
	Session  session.Session
	// History is every prior turn in this conversation, oldest first.
	// An empty History is the conversation's first turn.
	History []HistoryTurn
	// Message is the learner's new message this turn.
	Message string
	// RecentErrors/ConceptCandidates/ExpressionsToEncourage mirror
	// agent/teacher.ReviewInput's identically-named fields exactly —
	// see that type's doc comments — so application/conversation.Service
	// can build them the same way application/feedback.Service does.
	RecentErrors           []string
	ConceptCandidates      []string
	ExpressionsToEncourage []string
}

// promptData mirrors exactly what
// templates/conversation.turn.v1.*.md range/index over.
type promptData struct {
	TeacherMode            string
	Strictness             string
	ExplanationLanguage    string
	Purpose                string
	Register               string
	History                []HistoryTurn
	Message                string
	RecentErrors           []string
	ConceptCandidates      []string
	ExpressionsToEncourage []string
}

// The following DTOs mirror schemas/defs/conversation_turn.v1.json
// field-for-field, so unmarshaling a schema-valid response can never
// silently drop or misname a field. explanationDTO/correctionDTO are
// the same shape agent/teacher's own DTOs use for
// correction_result.v2's item — kept as separate types (not imported
// from that package) since agents never import one another.
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

type turnDTO struct {
	Reply       string          `json:"reply"`
	ReplyEN     string          `json:"reply_en,omitempty"`
	Corrections []correctionDTO `json:"corrections,omitempty"`
	Followup    string          `json:"followup,omitempty"`
}

// Turn is one AI-generated conversation turn: Reply is what the tutor
// says back (Japanese), ReplyEN an optional English gloss, Corrections
// whatever genuine mistakes it found in the learner's message (each
// assigned a fresh ID here — the same "the agent hands out IDs before
// anything is persisted" convention correction.NewResult uses for
// writing corrections), and Followup an optional question to keep the
// dialogue going.
type Turn struct {
	Reply       string
	ReplyEN     string
	Corrections []correction.Correction
	Followup    string
}

// Turn renders the conversation.turn prompt for in, asks gen for a
// conversation_turn.v1-shaped response (Rule 4: schema validation with
// one constrained-repair attempt and one full retry before failing),
// then maps the result onto Turn.
func (a *Agent) Turn(ctx context.Context, in TurnInput) (Turn, ai.StructuredResponse, error) {
	data := promptData{
		TeacherMode:            in.Session.Profile.TeacherMode,
		Strictness:             in.Session.Profile.Strictness,
		ExplanationLanguage:    in.Session.Profile.ExplanationLanguage,
		Purpose:                in.Session.Purpose,
		Register:               in.Session.Profile.Register,
		History:                in.History,
		Message:                in.Message,
		RecentErrors:           in.RecentErrors,
		ConceptCandidates:      in.ConceptCandidates,
		ExpressionsToEncourage: in.ExpressionsToEncourage,
	}
	rendered, err := prompts.Render(promptName, promptVersion, data)
	if err != nil {
		return Turn{}, ai.StructuredResponse{}, fmt.Errorf("conversation: render prompt: %w", err)
	}

	schema, err := schemas.Get(schemaName)
	if err != nil {
		return Turn{}, ai.StructuredResponse{}, fmt.Errorf("conversation: get schema: %w", err)
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
		return Turn{}, resp, fmt.Errorf("conversation: generate: %w", err)
	}

	resp, err = aiutil.ValidateWithRepairAndRetry(ctx, a.gen, schemaName, req, resp)
	if err != nil {
		return Turn{}, resp, err
	}

	var dto turnDTO
	if err := json.Unmarshal(resp.JSON, &dto); err != nil {
		return Turn{}, resp, fmt.Errorf("conversation: unmarshal turn: %w", err)
	}

	turn := Turn{
		Reply:       dto.Reply,
		ReplyEN:     dto.ReplyEN,
		Followup:    dto.Followup,
		Corrections: toCorrections(dto.Corrections),
	}
	return turn, resp, nil
}

func toCorrections(dtos []correctionDTO) []correction.Correction {
	cs := make([]correction.Correction, 0, len(dtos))
	for _, c := range dtos {
		cc := correction.Correction{
			ID:          uuid.New().String(),
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
	return cs
}
