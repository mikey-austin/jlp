package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ExerciseAttempt is one exercise_attempts row: the learner's response
// to a generated Exercise, its evaluation (deterministic or AI), and
// their self-rated confidence — see the 00013 migration. Confidence is
// nil when not given: application/practice.Service.Answer's confidence
// parameter uses 0 as its own "not given" sentinel (never persisted as
// a literal 0), converting it to a nil Confidence here, mirroring
// CorrectionRecord.Confidence's *int shape.
type ExerciseAttempt struct {
	ID                     string
	ExerciseID             string
	Response               string
	Correct                bool
	Score                  int
	FeedbackJA, FeedbackEN string
	Confidence             *int
	CreatedAt              time.Time
}

// ExerciseRepository persists generated drill exercises and the
// learner's attempts at them: exercises, exercise_attempts (see the
// 00013 migration). Unlike FeedbackRepository's single
// InsertFeedback-does-everything call, Create and RecordAttempt are two
// separate calls: application/practice.Service.Start only ever writes an
// exercises row, and Answer only ever writes an exercise_attempts row —
// there's no single transaction spanning "generate" and "answer" the
// way a feedback request's corrections all belong to one round-trip.
type ExerciseRepository interface {
	// Create persists ex, which the caller
	// (application/practice.Service.Start) has already fully populated,
	// including ID and CreatedAt.
	Create(ctx context.Context, ex exercise.Exercise) error
	// Get is identity-scoped: an exercise ID that exists but belongs to a
	// different identity misses with ErrNotFound, same as every other
	// identity-scoped repository in this package — this is what makes
	// application/practice.Service.Answer's cross-identity check work.
	Get(ctx context.Context, identity learner.IdentityID, id string) (exercise.Exercise, error)
	// RecordAttempt persists at, which the caller
	// (application/practice.Service.Answer) has already fully populated,
	// including ID and CreatedAt.
	RecordAttempt(ctx context.Context, at ExerciseAttempt) error
}
