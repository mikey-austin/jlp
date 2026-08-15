package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Lesson is one AI-prepared human-tutor lesson guide (PRD §18, §60):
// Plan is the validated lesson_plan.v1 JSON document verbatim, exactly
// as internal/agent/lesson.Agent.Generate produced it — this package
// never parses it apart; the HTTP layer's detail template does. Status
// starts "prepared" (right after Generate persists it) and moves to
// "completed" exactly once, via Complete, when the tutor session it was
// prepared for has actually happened and the tutor's observation has
// been recorded. CompletedAt is the zero time.Time until then.
type Lesson struct {
	ID          string
	IdentityID  learner.IdentityID
	Plan        []byte
	Status      string // "prepared" | "completed"
	CreatedAt   time.Time
	CompletedAt time.Time
}

// LessonObservation is one human tutor's post-lesson note (PRD §60): a
// free-text Notes field plus Subjects, a caller-supplied list of
// concept slugs or free-form tags the tutor called out (e.g.
// "i-adjective-past") — recorded alongside the lesson's completion but
// deliberately NOT yet consumed by the learner model (see
// application/lessons.Service.Complete's doc comment for why): it
// enriches the event log's history/rebuild fidelity for a future task,
// not this one.
type LessonObservation struct {
	ID        string
	LessonID  string
	Author    string
	Notes     string
	Subjects  []string
	CreatedAt time.Time
}

// LessonRepository persists tutor lesson guides and their post-lesson
// observations. Every method except Insert is identity-scoped: a
// lesson ID (or, for AddObservation/Observations, a lesson ID reached
// via one) that exists but belongs to a different identity misses with
// ErrNotFound — the same "wrong identity or unknown ID both miss the
// same way" contract every other identity-scoped repository in this
// package uses (see e.g. FeedbackRepository.GetCorrection).
type LessonRepository interface {
	// Insert persists a newly generated lesson, Status "prepared".
	Insert(ctx context.Context, l Lesson) error
	// List returns identity's lessons, newest first.
	List(ctx context.Context, identity learner.IdentityID) ([]Lesson, error)
	// Get reads back one lesson.
	Get(ctx context.Context, identity learner.IdentityID, id string) (Lesson, error)
	// Complete sets id's Status to "completed" and CompletedAt to the
	// current time, returning the updated lesson. Called once per lesson
	// — application/lessons.Service.Complete calls this alongside
	// AddObservation below, but the two are separate persistence
	// operations (see that method's doc comment for why no single
	// transaction spans them).
	Complete(ctx context.Context, identity learner.IdentityID, id string) (Lesson, error)
	// AddObservation persists o (o.LessonID identifies the target
	// lesson) only if that lesson actually belongs to identity — checked
	// via a join to the lessons table itself, mirroring
	// AIRatingRepository.Upsert's "never trust the caller, check via a
	// join" shape. A lesson ID that doesn't exist, or exists but belongs
	// to a different identity, both miss with ErrNotFound.
	AddObservation(ctx context.Context, identity learner.IdentityID, o LessonObservation) error
	// Observations returns every observation recorded against lessonID,
	// newest first, scoped via the same join to lessons AddObservation
	// uses: a lessonID that doesn't exist, or belongs to a different
	// identity, both come back an empty slice (no error) — the caller
	// (application/lessons.Service.Detail via the HTTP handler) always
	// calls Get first, which DOES return ErrNotFound for either case, so
	// by the time Observations runs the lesson's ownership is already
	// established; this mirrors AnkiCardRepository.List's identity-
	// filtered-not-errored convention for a read-only listing.
	Observations(ctx context.Context, identity learner.IdentityID, lessonID string) ([]LessonObservation, error)
}
