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
	appretrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
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

// dueSubjectsScanLimit caps how many of the scheduler's most-due items
// Start scans looking for the first "concept" one (see dueConcept):
// DueSubjects can return "expression" items too (vocabulary due for
// review), which Start has no use for, so it looks a little past the
// single most-due item rather than falling back to the planner just
// because the very top of the queue happens to be an expression.
const dueSubjectsScanLimit = 5

// recentWordWindow is how far back a word counts as "recently added"
// and therefore drillable ahead of anything the scheduler has queued.
//
// Two weeks, because that is roughly how long the context that produced
// a word survives — the sentence it came from, why it was looked up.
// Past that it is just another item, and the spaced scheduler is the
// better judge of when to show it.
const recentWordWindow = 14 * 24 * time.Hour

// recentWordScanLimit caps how many recent words Start considers. It
// takes the newest and drills that one; the cap exists so a bulk import
// of 500 words is one bounded query rather than a full scan.
const recentWordScanLimit = 20

// Service wires the drill pipeline. planner and grammar are used ONLY
// by Start, to decide what concept to drill next (see TopConcept and
// randomCatalogConcept below) — Answer never touches them. retrieval is
// also Start-only (see dueConcept): a due subject takes priority over
// the planner's own top concept (PRD §54's queue is meant to actually
// resurface, not just sit unread on /learner).
type Service struct {
	repo      storage.ExerciseRepository
	agent     *drill.Agent
	planner   *planner.Planner
	grammar   storage.GrammarRepository
	rec       *learning.Recorder
	retrieval *appretrieval.Scheduler
	// vocab is Start-only, and nil-tolerant: a caller that has not wired
	// it simply never gets word drills, exactly as a nil retrieval means
	// nothing is ever due.
	vocab storage.VocabularyRepository
	now   func() time.Time
}

// NewService wires the practice pipeline.
func NewService(repo storage.ExerciseRepository, agent *drill.Agent, plnr *planner.Planner, grammarRepo storage.GrammarRepository, rec *learning.Recorder, retrieval *appretrieval.Scheduler, vocab storage.VocabularyRepository, now func() time.Time) *Service {
	if now == nil {
		now = time.Now
	}
	return &Service{repo: repo, agent: agent, planner: plnr, grammar: grammarRepo, rec: rec, retrieval: retrieval, vocab: vocab, now: now}
}

// Start generates and persists one new exercise for identity, picking
// what to drill in priority order:
//  1. a recently added word the learner has never produced correctly
//     (recentWord) — newest first. A word added this week still has the
//     context that produced it attached, which is when it is cheapest
//     to learn; the spaced scheduler is the better judge of everything
//     older;
//  2. a concept due for spaced review (dueConcept, PRD §54) —
//     resurfacing something the learner is about to forget beats
//     drilling a fresh weakness;
//  3. the planner's top concept (planner.Planner.TopConcept) when
//     identity has a live grammar weakness and nothing is due;
//  4. a random catalog concept otherwise (a brand-new learner, or one
//     whose top priority isn't concept-type).
//
// Step 1 is the only step that produces a word drill, and it needs no
// model call at all: the reading and meaning are already stored, so the
// exercise is built directly from the item (see wordRecall).
//
// providerOverride, when non-empty, names the AI adapter that must
// generate this drill — the 練習 page's dropdown, for this request only.
// It is passed straight to drill.GenerateInput and never persisted.
// Empty means "route normally", which is what every non-interactive
// caller (the channel adapters) wants: nobody is there to pick.
//
// Records quiz.started with Evidence {"concept":…, "type":…}.
func (s *Service) Start(ctx context.Context, identity learner.IdentityID, providerOverride string) (exercise.Exercise, error) {
	// A recent word short-circuits everything below, including the model
	// call: nothing to generate, so nothing to wait for or pay for.
	if item, ok, err := s.recentWord(ctx, identity); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: recent word: %w", err)
	} else if ok {
		return s.persist(ctx, identity, wordRecall(item))
	}

	concept, ok, err := s.dueConcept(ctx, identity)
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: due concept: %w", err)
	}
	if !ok {
		concept, ok, err = s.planner.TopConcept(ctx, identity)
		if err != nil {
			return exercise.Exercise{}, fmt.Errorf("practice: top concept: %w", err)
		}
	}
	if !ok {
		concept, err = s.randomCatalogConcept(ctx)
		if err != nil {
			return exercise.Exercise{}, err
		}
	}

	ex, _, err := s.agent.Generate(ctx, drill.GenerateInput{
		Identity:         identity,
		Concept:          concept,
		ProviderOverride: providerOverride,
	})
	if err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: generate exercise: %w", err)
	}
	ex.SubjectType = exercise.SubjectConcept
	ex.SubjectRef = concept.Slug

	return s.persist(ctx, identity, ex)
}

// persist stamps, stores and announces a freshly chosen exercise —
// shared by the word path and the generated path so the two cannot
// drift on what a started drill records.
func (s *Service) persist(ctx context.Context, identity learner.IdentityID, ex exercise.Exercise) (exercise.Exercise, error) {
	// ID/CreatedAt are assigned here, at persistence time — not by the
	// Drill agent (see drill.Agent.Generate's own doc comment on why).
	ex.ID = uuid.New().String()
	ex.IdentityID = identity
	ex.CreatedAt = s.now().UTC()

	if err := s.repo.Create(ctx, ex); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: persist exercise: %w", err)
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeQuizStarted,
		Subject:    ex.ID,
		Evidence: map[string]any{
			"concept":      ex.ConceptSlug,
			"type":         ex.Type,
			"subject_type": ex.SubjectType,
			"subject_ref":  ex.SubjectRef,
		},
	}); err != nil {
		return exercise.Exercise{}, fmt.Errorf("practice: record %s: %w", event.TypeQuizStarted, err)
	}

	return ex, nil
}

// recentWord returns the newest word identity added inside
// recentWordWindow and has never produced correctly.
//
// ok is false — with no error — when vocab is nil (a caller that hasn't
// wired it) or nothing qualifies, exactly as dueConcept reports "nothing
// due".
func (s *Service) recentWord(ctx context.Context, identity learner.IdentityID) (vocabulary.Item, bool, error) {
	if s.vocab == nil {
		return vocabulary.Item{}, false, nil
	}
	items, err := s.vocab.ListRecentUnpracticed(ctx, identity, s.now().UTC().Add(-recentWordWindow), recentWordScanLimit)
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	for _, item := range items {
		// A word with no reading AND no meaning has nothing on the back
		// of the card. Skipped rather than shown, since a flip card that
		// reveals nothing teaches nothing.
		if strings.TrimSpace(item.Reading) != "" || strings.TrimSpace(item.Meaning) != "" || strings.TrimSpace(item.MeaningEN) != "" {
			return item, true, nil
		}
	}
	return vocabulary.Item{}, false, nil
}

// wordRecall builds a flip card from a stored vocabulary item.
//
// No model call: the reading and meaning are already known, so asking
// one to restate them would add latency, cost, and a chance of being
// wrong about the learner's own vocabulary. Answer holds the reading so
// the typed-answer path can still grade it deterministically; the card
// itself is self-graded (see Answer's word-recall branch).
func wordRecall(item vocabulary.Item) exercise.Exercise {
	back := strings.TrimSpace(item.Meaning)
	if back == "" {
		back = strings.TrimSpace(item.MeaningEN)
	}
	return exercise.Exercise{
		SubjectType:    exercise.SubjectWord,
		SubjectRef:     item.ID,
		Type:           exercise.TypeWordRecall,
		InstructionsJA: "この語の読みと意味を思い出してください。",
		InstructionsEN: "Recall this word's reading and meaning, then check yourself.",
		Prompt:         item.Expression,
		Answer:         strings.TrimSpace(item.Reading),
		Acceptable:     []string{back},
	}
}

// dueConcept looks for a concept Start should drill because it's due
// for spaced review (PRD §54): the identity's most-due items
// (retrieval.Scheduler.DueSubjects, scanned up to dueSubjectsScanLimit
// deep), returning the first one whose SubjectType is "concept",
// resolved to a grammar.Concept the same way TopConcept resolves its
// own subject. ok is false — with no error — when retrieval is nil (a
// caller that hasn't wired the scheduler, e.g. some existing tests),
// nothing is due, only "expression" items are due, or the due
// concept's slug no longer resolves (storage.ErrNotFound from
// GetConcept — the same stale-slug tolerance TopConcept already has).
// Any other DueSubjects or GetConcept failure propagates as an error.
func (s *Service) dueConcept(ctx context.Context, identity learner.IdentityID) (grammar.Concept, bool, error) {
	if s.retrieval == nil {
		return grammar.Concept{}, false, nil
	}
	due, err := s.retrieval.DueSubjects(ctx, identity, dueSubjectsScanLimit)
	if err != nil {
		return grammar.Concept{}, false, err
	}
	for _, item := range due {
		if item.SubjectType != "concept" {
			continue
		}
		concept, err := s.grammar.GetConcept(ctx, item.Subject)
		if err != nil {
			if errors.Is(err, storage.ErrNotFound) {
				continue
			}
			return grammar.Concept{}, false, fmt.Errorf("get concept %q: %w", item.Subject, err)
		}
		return concept, true, nil
	}
	return grammar.Concept{}, false, nil
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
