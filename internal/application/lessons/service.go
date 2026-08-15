// Package lessons implements the human-tutor lesson guide pipeline
// (Phase 3 Task 4, PRD §18, §60): it gathers a learner's current
// context (scored priorities, dormant vocabulary, recent writing
// corrections, and learner-model observations), asks the lesson agent
// to write a lesson_plan.v1 guide from it, persists the result, and
// records the tutor.lesson.created/tutor.lesson.completed events.
package lessons

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/agent/lesson"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/application/planner"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// prioritiesLimit/activationLimit/correctionsLimit cap how much
// context Generate hands the lesson agent — the brief's "Top(8)
// priorities, ActivationCandidates(8), recent correction pairs ...
// limit" (a lesson guide is meant to be skimmed by a tutor before a
// session, not an exhaustive dump of the learner's whole history).
const (
	prioritiesLimit  = 8
	activationLimit  = 8
	correctionsLimit = 8
)

// Service wires the tutor lesson guide pipeline.
type Service struct {
	repo     storage.LessonRepository
	prios    storage.PriorityRepository
	planner  *planner.Planner
	feedback storage.FeedbackRepository
	obs      storage.ObservationRepository
	agent    *lesson.Agent
	rec      *learning.Recorder
}

// NewService wires the tutor lesson guide pipeline.
func NewService(repo storage.LessonRepository, prios storage.PriorityRepository, plnr *planner.Planner, feedback storage.FeedbackRepository, obs storage.ObservationRepository, agent *lesson.Agent, rec *learning.Recorder) *Service {
	return &Service{repo: repo, prios: prios, planner: plnr, feedback: feedback, obs: obs, agent: agent, rec: rec}
}

// Generate gathers identity's current context — Top(8) priorities
// (application/planner's heuristic teaching planner output),
// ActivationCandidates(8) (dormant vocabulary ripe for encouragement),
// RecentCorrections(8) (the learner's own recent writing mistakes), and
// every standing learner-model observation — asks the lesson agent to
// write a lesson_plan.v1 guide from it, persists the result Status
// "prepared", and records tutor.lesson.created. A generation or
// persistence failure is a hard error (nothing durable to preserve
// yet); a subsequent event-record failure is logged and continues (the
// lesson is already durably persisted by then), matching
// application/anki.Service.GenerateFromCorrection's same tradeoff.
func (s *Service) Generate(ctx context.Context, identity learner.IdentityID) (storage.Lesson, error) {
	priorities, err := s.prios.Top(ctx, identity, prioritiesLimit)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: top priorities: %w", err)
	}

	activation, err := s.planner.ActivationCandidates(ctx, identity, activationLimit)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: activation candidates: %w", err)
	}

	corrections, err := s.feedback.RecentCorrections(ctx, identity, correctionsLimit)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: recent corrections: %w", err)
	}

	observations, err := s.obs.List(ctx, identity)
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: list observations: %w", err)
	}

	planJSON, _, err := s.agent.Generate(ctx, lesson.GenerateInput{
		Identity:              identity,
		Priorities:            formatPriorities(priorities),
		ActivationExpressions: formatExpressions(activation),
		RecentCorrections:     formatCorrections(corrections),
		ObservationSummaries:  formatObservations(observations),
	})
	if err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: generate: %w", err)
	}

	l := storage.Lesson{
		ID:         uuid.New().String(),
		IdentityID: identity,
		Plan:       planJSON,
		Status:     "prepared",
		CreatedAt:  time.Now().UTC(),
	}
	if err := s.repo.Insert(ctx, l); err != nil {
		return storage.Lesson{}, fmt.Errorf("lessons: persist: %w", err)
	}

	// Log-and-continue, not hard-fail: l is already durably persisted
	// above — see application/anki.Service.GenerateFromCorrection's own
	// event-recording doc comment for the same reasoning.
	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeTutorLessonCreated,
		Subject:    l.ID,
		Evidence: map[string]any{
			"priorities":  len(priorities),
			"corrections": len(corrections),
		},
	}); err != nil {
		slog.Error("record tutor.lesson.created", "identity", identity, "lesson", l.ID, "err", err)
	}

	return l, nil
}

// Complete records a human tutor's post-lesson observation (author,
// free-text notes, and a caller-supplied list of subjects — concept
// slugs or free-form tags) and marks lessonID "completed", atomically
// (storage.LessonRepository.CompleteWithObservation — see its doc
// comment for why a partial write here is unacceptable, not just
// untidy: the detail template only renders observations once Status is
// "completed", so an observation attached without the status flip
// would be durably persisted yet permanently invisible, and a retry
// would then attach a second, duplicate observation), then records
// tutor.lesson.completed with Evidence carrying subjects. A wrong
// identity or unknown lessonID both propagate storage.ErrNotFound with
// NEITHER the observation nor the status change taking effect.
//
// The learner model does NOT yet consume tutor.lesson.completed —
// unlike correction.presented/grammar.concept.encountered, no consumer
// subscribes to it in cmd/jlp/main.go. It exists purely to enrich the
// activity/event history and a future learnermodel rebuild's fidelity
// (a later task may teach the learner model to weigh a tutor's
// first-hand observation), matching TypeAnkiCardCreated/
// TypeAnkiCardExported's same "recorded now, consumed later or never"
// shape.
func (s *Service) Complete(ctx context.Context, identity learner.IdentityID, lessonID, author, notes string, subjects []string) (storage.Lesson, error) {
	obs := storage.LessonObservation{
		ID:        uuid.New().String(),
		LessonID:  lessonID,
		Author:    author,
		Notes:     notes,
		Subjects:  subjects,
		CreatedAt: time.Now().UTC(),
	}
	l, err := s.repo.CompleteWithObservation(ctx, identity, lessonID, obs, time.Now().UTC())
	if err != nil {
		return storage.Lesson{}, err
	}

	if err := s.rec.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeTutorLessonCompleted,
		Subject:    lessonID,
		Evidence:   map[string]any{"subjects": subjects},
	}); err != nil {
		slog.Error("record tutor.lesson.completed", "identity", identity, "lesson", lessonID, "err", err)
	}

	return l, nil
}

// formatPriorities mirrors application/feedback.Service.recentErrors'
// formatting exactly, so a tutor and the Teacher agent see priorities
// phrased the same way.
func formatPriorities(ps []storage.Priority) []string {
	lines := make([]string, 0, len(ps))
	for _, p := range ps {
		lines = append(lines, fmt.Sprintf("%s (%s weakness, score %.1f): %s", p.Subject, p.SubjectType, p.Score, p.Reason))
	}
	return lines
}

// formatExpressions mirrors application/feedback.Service.
// expressionsToEncourage's formatting exactly.
func formatExpressions(items []vocabulary.Item) []string {
	lines := make([]string, 0, len(items))
	for _, item := range items {
		if item.Reading != "" && item.Reading != item.Expression {
			lines = append(lines, fmt.Sprintf("%s (%s) — %s", item.Expression, item.Reading, item.Meaning))
		} else {
			lines = append(lines, fmt.Sprintf("%s — %s", item.Expression, item.Meaning))
		}
	}
	return lines
}

// formatCorrections turns each recent correction into one tutor-
// readable "original → replacement (type)" line.
func formatCorrections(cs []storage.CorrectionRecord) []string {
	lines := make([]string, 0, len(cs))
	for _, c := range cs {
		lines = append(lines, fmt.Sprintf("%s → %s (%s)", c.Original, c.Replacement, c.Type))
	}
	return lines
}

// formatObservations turns each learner-model observation into one
// tutor-readable "subject (kind, subject-type): confidence N.N" line.
func formatObservations(obs []learnermodel.Observation) []string {
	lines := make([]string, 0, len(obs))
	for _, o := range obs {
		lines = append(lines, fmt.Sprintf("%s (%s, %s): confidence %.1f", o.Subject, o.Kind, o.SubjectType, o.Confidence))
	}
	return lines
}
