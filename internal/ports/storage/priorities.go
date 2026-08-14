package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Priority is one row of the teaching planner's output: how urgently
// identity should be taught about (SubjectType, Subject), and why.
// Produced entirely by internal/application/planner.Recompute — never
// written by hand — and consumed by internal/application/feedback's
// Teacher prompt (as RecentErrors) and the /learner page.
type Priority struct {
	IdentityID  learner.IdentityID
	SubjectType string // "concept" | "correction-type"
	Subject     string
	Score       float64
	Reason      string // human-readable, e.g. "recurring weakness: 5 occurrences in 30d"
	UpdatedAt   time.Time
}

// PriorityRepository persists the planner's output: learner_priorities,
// PRIMARY KEY (identity_id, subject_type, subject) — see the 00010
// migration.
type PriorityRepository interface {
	// ReplaceAll atomically swaps identity's whole priority list: delete
	// every existing row for identity, then insert ps, in one
	// transaction — a Recompute mid-write never leaves a stale row from
	// a subject that's since dropped off the list, nor a torn mix of old
	// and new scores.
	ReplaceAll(ctx context.Context, identity learner.IdentityID, ps []Priority) error
	// Top returns identity's highest-scoring priorities, at most limit,
	// score DESC.
	Top(ctx context.Context, identity learner.IdentityID, limit int) ([]Priority, error)
}
