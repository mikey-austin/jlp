package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// RetrievalItem is one (identity, subjectType, subject) spaced-retrieval
// schedule row (PRD §54): application/retrieval.Scheduler is the only
// writer of Interval/DueAt — see its RecordOutcome for the interval
// policy that computes them.
type RetrievalItem struct {
	IdentityID learner.IdentityID
	// SubjectType is "concept" (a grammar concept slug, from
	// quiz.answered/correction.retried) or "expression" (a vocabulary
	// expression, from vocabulary.produced-correctly).
	SubjectType string
	Subject     string
	Successes   int
	Failures    int
	LastSeen    time.Time
	DueAt       time.Time
	Interval    time.Duration
	// Confidence is the last reported confidence (1..5), or 0 when the
	// outcome that last updated this row carried none.
	Confidence float64
}

// RetrievalRepository persists spaced-retrieval schedules: one row per
// (identity, subject_type, subject), UNIQUE — see the migration that
// creates retrieval_items and db/queries/retrieval.sql for the ON
// CONFLICT replace semantics Upsert relies on. Every method is
// identity-scoped: a subject that exists but belongs to a different
// identity misses exactly like a subject that was never scheduled at
// all (ErrNotFound from Get, an empty/filtered result from Due/List) —
// never another identity's data.
type RetrievalRepository interface {
	// Upsert inserts it, or — if a row already exists for
	// (it.IdentityID, it.SubjectType, it.Subject) — replaces its
	// Successes/Failures/LastSeen/DueAt/Interval/Confidence in place.
	Upsert(ctx context.Context, it RetrievalItem) error
	// Get reads back identity's single schedule row for
	// (subjectType, subject). Misses with ErrNotFound — including when
	// the row exists but belongs to a different identity.
	Get(ctx context.Context, identity learner.IdentityID, subjectType, subject string) (RetrievalItem, error)
	// Due returns identity's items whose DueAt is at or before at,
	// ordered by DueAt ascending (most overdue first) and capped at
	// limit (limit <= 0 means unlimited).
	Due(ctx context.Context, identity learner.IdentityID, at time.Time, limit int) ([]RetrievalItem, error)
	// List returns every one of identity's schedule rows, ordered by
	// DueAt ascending regardless of whether they're due yet — the
	// /learner 復習キュー table's full upcoming-review view — capped at
	// limit (limit <= 0 means unlimited).
	List(ctx context.Context, identity learner.IdentityID, limit int) ([]RetrievalItem, error)
}
