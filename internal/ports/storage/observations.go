package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
)

// ObservationRepository persists the learner model: one row per
// (identity, subject_type, subject), maintained entirely by
// internal/application/learnermodel's event consumers and Rebuild.
type ObservationRepository interface {
	// Upsert inserts o, or — if a row already exists for
	// (o.IdentityID, o.SubjectType, o.Subject), the UNIQUE constraint the
	// 00009 migration puts on that triple — replaces its Kind,
	// Confidence, Evidence, and UpdatedAt in place. o.ID and o.FirstSeen
	// are used only for a fresh insert; an existing row keeps its
	// original id and first_seen.
	Upsert(ctx context.Context, o learnermodel.Observation) error
	// List returns every observation for identity, in no particular
	// order guaranteed beyond what the adapter documents.
	List(ctx context.Context, identity learner.IdentityID) ([]learnermodel.Observation, error)
	// DeleteAll removes every observation for identity — the first step
	// of a rebuild, so a replay starts from a clean slate.
	DeleteAll(ctx context.Context, identity learner.IdentityID) error
}
