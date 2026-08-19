package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	appanki "github.com/mikeyaustin/jlp/internal/application/anki"
	applearning "github.com/mikeyaustin/jlp/internal/application/learning"
	applessons "github.com/mikeyaustin/jlp/internal/application/lessons"
	appractice "github.com/mikeyaustin/jlp/internal/application/practice"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	domsession "github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// validEventTypes is built once from event.AllTypes() — the same
// enumeration internal/domain/event's own drift test keeps honest —
// so record_learning_event can refuse a model-supplied type that
// names no real event, rather than silently appending a garbage row
// to the append-only learning_events log.
var validEventTypes = func() map[string]bool {
	set := make(map[string]bool, len(event.AllTypes()))
	for _, t := range event.AllTypes() {
		set[string(t)] = true
	}
	return set
}()

// LearningTools returns the "learning" group's tools (PRD §29): the
// four WRITE tools — record_learning_event, create_exercise,
// create_lesson_plan, create_anki_card — every one delegating to the
// same application service the HTTP layer uses, so every invariant
// (event validation, the drill pipeline's concept selection, the
// lesson agent's context gathering, and — critically —
// create_anki_card's socratic gate) holds exactly as it does there.
func LearningTools(rec *applearning.Recorder, practice *appractice.Service, lessons *applessons.Service, anki *appanki.Service) []Tool {
	return []Tool{
		recordLearningEventTool(rec),
		createExerciseTool(practice),
		createLessonPlanTool(lessons),
		createAnkiCardTool(anki),
	}
}

type recordLearningEventArgs struct {
	Type     string         `json:"type"`
	Subject  string         `json:"subject"`
	Evidence map[string]any `json:"evidence"`
}

func recordLearningEventTool(rec *applearning.Recorder) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "record_learning_event",
			Description: "Records one learning event to the learner's append-only history. type must be one of the system's known event types (e.g. \"grammar.concept.encountered\", \"vocabulary.looked-up\"); subject and evidence are optional.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"},"subject":{"type":"string"},"evidence":{"type":"object"}},"required":["type"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, sessionID *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[recordLearningEventArgs](args)
			if err != nil {
				return "", fmt.Errorf("record_learning_event: %w", err)
			}
			if !validEventTypes[a.Type] {
				return "", fmt.Errorf("record_learning_event: unknown event type %q", a.Type)
			}
			ev := event.LearningEvent{
				IdentityID: identity,
				SessionID:  sessionID,
				Type:       event.Type(a.Type),
				Subject:    a.Subject,
				Evidence:   a.Evidence,
			}
			if err := rec.Record(ctx, ev); err != nil {
				return "", fmt.Errorf("record_learning_event: %w", err)
			}
			return `{"recorded":true}`, nil
		},
	}
}

type exerciseView struct {
	ID             string   `json:"id"`
	ConceptSlug    string   `json:"concept_slug"`
	Type           string   `json:"type"`
	InstructionsEN string   `json:"instructions_en"`
	Prompt         string   `json:"prompt"`
	Choices        []string `json:"choices,omitempty"`
}

func createExerciseTool(svc *appractice.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "create_exercise",
			Description: "Generates and persists one new drill exercise for the learner, targeting their current top grammar weakness (or a random catalog concept if they have none). Takes no arguments.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, _ json.RawMessage) (string, error) {
			// No provider override: an agent calling this tool is already
			// running on whichever adapter the caller chose, and letting a
			// model pick its own would make the choice unauditable.
			ex, err := svc.Start(ctx, identity, appractice.StartOptions{})
			if err != nil {
				return "", fmt.Errorf("create_exercise: %w", err)
			}
			out, err := json.Marshal(exerciseView{
				ID:             ex.ID,
				ConceptSlug:    ex.ConceptSlug,
				Type:           ex.Type,
				InstructionsEN: ex.InstructionsEN,
				Prompt:         ex.Prompt,
				Choices:        ex.Choices,
			})
			if err != nil {
				return "", fmt.Errorf("create_exercise: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type lessonPlanView struct {
	ID     string          `json:"id"`
	Status string          `json:"status"`
	Plan   json.RawMessage `json:"plan"`
}

func createLessonPlanTool(svc *applessons.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "create_lesson_plan",
			Description: "Generates and persists a new human-tutor lesson guide from the learner's current priorities, dormant vocabulary, recent corrections, and learner-model observations. Takes no arguments.",
			Schema:      json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, _ json.RawMessage) (string, error) {
			lesson, err := svc.Generate(ctx, identity)
			if err != nil {
				return "", fmt.Errorf("create_lesson_plan: %w", err)
			}
			out, err := json.Marshal(lessonPlanView{ID: lesson.ID, Status: lesson.Status, Plan: json.RawMessage(lesson.Plan)})
			if err != nil {
				return "", fmt.Errorf("create_lesson_plan: encode: %w", err)
			}
			return string(out), nil
		},
	}
}

type createAnkiCardArgs struct {
	CorrectionID string `json:"correction_id"`
}

type ankiCardView struct {
	ID     string `json:"id"`
	Front  string `json:"front"`
	Back   string `json:"back"`
	Notes  string `json:"notes,omitempty"`
	Status string `json:"status"`
}

func createAnkiCardTool(svc *appanki.Service) Tool {
	return Tool{
		Def: ai.ToolDef{
			Name:        "create_anki_card",
			Description: "Generates and persists one Anki flashcard from a specific correction. correction_id is required. Refuses (returns an error, creates no card) if the correction is still gated behind its own socratic hint.",
			Schema:      json.RawMessage(`{"type":"object","properties":{"correction_id":{"type":"string"}},"required":["correction_id"],"additionalProperties":false}`),
		},
		Handler: func(ctx context.Context, identity learner.IdentityID, _ *domsession.ID, args json.RawMessage) (string, error) {
			a, err := decodeArgs[createAnkiCardArgs](args)
			if err != nil {
				return "", fmt.Errorf("create_anki_card: %w", err)
			}
			if a.CorrectionID == "" {
				return "", fmt.Errorf("create_anki_card: correction_id is required")
			}
			card, err := svc.GenerateFromCorrection(ctx, identity, a.CorrectionID)
			if err != nil {
				if errors.Is(err, appanki.ErrCorrectionGated) {
					return "", fmt.Errorf("create_anki_card: correction %q is still gated behind its socratic hint — the learner must reveal or resolve it first", a.CorrectionID)
				}
				return "", fmt.Errorf("create_anki_card: %w", err)
			}
			out, err := json.Marshal(ankiCardView{ID: card.ID, Front: card.Front, Back: card.Back, Notes: card.Notes, Status: card.Status})
			if err != nil {
				return "", fmt.Errorf("create_anki_card: encode: %w", err)
			}
			return string(out), nil
		},
	}
}
