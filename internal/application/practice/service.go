// Package practice is the application-layer drill pipeline (PRD §17.2,
// §58): it picks what to drill (the planner's top concept, or a random
// catalog concept as a fallback), asks the Drill agent to generate an
// exercise, persists it, scores a learner's answer — deterministically
// for typed answers, via the Drill agent for free production — and
// records the learning events the rest of the system reacts to.
package practice

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/drill"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ErrInvalidConfidence is returned when Answer is asked to record a
// confidence outside the documented 0 (not given) / 1..5 (valid) range.
var ErrInvalidConfidence = errors.New("practice: confidence must be 0 (not given) or between 1 and 5")

// Deterministic-path feedback copy (PRD §56: encouraging, no
// streaks/penalties — a wrong answer is an invitation to try again, not
// a scored failure). Free-production feedback instead comes from the
// Drill agent's own evaluation, whose drill.evaluate.v1 system prompt
// carries the same "teaches, never scolds" instruction.
const (
	correctFeedbackJA = "正解です！"
	correctFeedbackEN = "Correct!"
	wrongFeedbackJA   = "惜しい！もう一度挑戦しましょう。"
	wrongFeedbackEN   = "Not quite — let's try again."

	deterministicCorrectScore = 100
	deterministicWrongScore   = 0
)

// Service wires the drill pipeline. planner and grammar are used ONLY
// by Start, to decide what concept to drill next (see TopConcept and
// randomCatalogConcept below) — Answer never touches them.
type Service struct {
	repo    storage.ExerciseRepository
	agent   *drill.Agent
	planner *planner.Planner
	grammar storage.GrammarRepository
	rec     *learning.Recorder
}

// NewService wires the practice pipeline.
func NewService(repo storage.ExerciseRepository, agent *drill.Agent, plnr *planner.Planner, grammarRepo storage.GrammarRepository, rec *learning.Recorder) *Service {
	return &Service{repo: repo, agent: agent, planner: plnr, grammar: grammarRepo, rec: rec}
}

// Start generates and persists one new exercise for identity: the
// planner's top concept (planner.Planner.TopConcept) when identity has
// a live grammar weakness, or a random catalog concept otherwise (a
// brand-new learner, or one whose top priority isn't concept-type).
// Records quiz.started with Evidence {"concept":…, "type":…}.
func (s *Service) Start(ctx context.Context, identity learner.IdentityID) (exercise.Exercise, error) {
	concept, ok, err := s.planner.TopConcept(ctx, identity)
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: top concept: %w", err)
	}
	if !ok {
		concept, err = s.randomCatalogConcept(ctx)
		if err != nil {
			return exercise.Exercise{}, err
		}
	}

	ex, _, err := s.agent.Generate(ctx, drill.GenerateInput{Identity: identity, Concept: concept})
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: generate exercise: %w", err)
	}

	// ID/CreatedAt are assigned here, at persistence time — not by the
	// Drill agent (see drill.Agent.Generate's own doc comment on why).
	ex.ID = uuid.New().String()
	ex.CreatedAt = time.Now().UTC()

	if err := s.repo.Create(ctx, ex); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: persist exercise: %w", err)
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizStarted,
		Subject:    ex.ID,
		Evidence: map[string]any{
			"concept": ex.ConceptSlug,
			"type":    ex.Type,
		},
	}); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizStarted, err)
	}

	return ex, nil
}

// randomCatalogConcept picks a uniformly random concept from the full
// grammar catalog — Start's fallback when TopConcept has nothing to
// offer. Using math/rand's global source (not injected) is deliberate:
// unlike planner.Planner's injectable clock, Start's tests pin this path
// with a single-concept catalog fixture (rand.Intn(1) is always 0), so
// there's no need to make the choice itself deterministic to test it.
func (s *Service) randomCatalogConcept(ctx context.Context) (grammar.Concept, error) {
	concepts, err := s.grammar.ListConcepts(ctx)
	if err != nil {
		return grammar.Concept{}, fmt.Errorf("practice: list grammar concepts: %w", err)
	}
	if len(concepts) == 0 {
		return grammar.Concept{}, errors.New("practice: no grammar concepts available to drill")
	}
	return concepts[rand.Intn(len(concepts))], nil //nolint:gosec // not a security-sensitive choice
}

// Answer scores identity's response to a previously generated exercise,
// persists the attempt, and records quiz.answered + quiz.completed
// (Evidence {"concept":…, "type":…, "correct":…, "confidence":…} on
// both — see this package's tests for why they carry the same shape).
//
// exerciseID is looked up via repo.Get, which is identity-scoped: an
// exercise ID that exists but belongs to a different identity misses
// with storage.ErrNotFound, propagated unwrapped so callers can
// errors.Is against it exactly like every other identity-scoped lookup
// in this codebase.
//
// confidence is the learner's optional self-rating: 0 means "not
// given" (never persisted as a literal 0 — see
// storage.ExerciseAttempt.Confidence's *int/nil shape), 1..5 is valid,
// anything else is ErrInvalidConfidence.
func (s *Service) Answer(ctx context.Context, identity learner.IdentityID, exerciseID, response string, confidence int) (exercise.Evaluation, error) {
	if confidence != 0 && (confidence < 1 || confidence > 5) {
		return exercise.Evaluation{}, fmt.Errorf("%w: got %d", ErrInvalidConfidence, confidence)
	}

	ex, err := s.repo.Get(ctx, identity, exerciseID)
	if err != nil {
		return exercise.Evaluation{}, err
	}

	var eval exercise.Evaluation
	if ex.Type == exercise.TypeFreeProduction {
		eval, _, err = s.agent.Evaluate(ctx, identity, ex, response)
		if err != nil {
			return exercise.Evaluation{}, fmt.Errorf("practice: evaluate: %w", err)
		}
	} else {
		eval = deterministicEvaluation(response, ex)
	}

	var confidencePtr *int
	if confidence != 0 {
		c := confidence
		confidencePtr = &c
	}
	if err := s.repo.RecordAttempt(ctx, storage.ExerciseAttempt{
		ID:         uuid.New().String(),
		ExerciseID: exerciseID,
		Response:   response,
		Correct:    eval.Correct,
		Score:      eval.Score,
		FeedbackJA: eval.FeedbackJA,
		FeedbackEN: eval.FeedbackEN,
		Confidence: confidencePtr,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record attempt: %w", err)
	}

	evidence := map[string]any{
		"concept":    ex.ConceptSlug,
		"type":       ex.Type,
		"correct":    eval.Correct,
		"confidence": confidence,
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizAnswered,
		Subject:    exerciseID,
		Evidence:   evidence,
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizAnswered, err)
	}
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizCompleted,
		Subject:    exerciseID,
		Evidence:   evidence,
	}); err != nil {
		return exercise.Evaluation{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizCompleted, err)
	}

	return eval, nil
}

// deterministicEvaluation implements the brief's deterministic scoring
// path for fill-in-blank/multiple-choice/transformation exercises:
// TrimSpace rune-equal comparison of response against ex.Answer or any
// ex.Acceptable — never the AI. Feedback copy is fixed and encouraging
// either way (PRD §56): no streak language, no penalty language, a
// wrong answer just invites another attempt.
func deterministicEvaluation(response string, ex exercise.Exercise) exercise.Evaluation {
	trimmed := strings.TrimSpace(response)
	correct := trimmed == strings.TrimSpace(ex.Answer)
	if !correct {
		for _, a := range ex.Acceptable {
			if trimmed == strings.TrimSpace(a) {
				correct = true
				break
			}
		}
	}
	if correct {
		return exercise.Evaluation{Correct: true, Score: deterministicCorrectScore, FeedbackJA: correctFeedbackJA, FeedbackEN: correctFeedbackEN}
	}
	return exercise.Evaluation{Correct: false, Score: deterministicWrongScore, FeedbackJA: wrongFeedbackJA, FeedbackEN: wrongFeedbackEN}
}
