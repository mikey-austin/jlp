package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// LearningEventRepository stores immutable, append-only learning events.
// Rows are never updated or deleted; ListRecent is identity-scoped like the
// other repositories, with an optional session filter.
type LearningEventRepository interface {
	Append(ctx context.Context, ev event.LearningEvent) error
	// ListRecent returns up to limit events for identity, newest first. If
	// sid is non-nil, results are further scoped to that session.
	ListRecent(ctx context.Context, identity learner.IdentityID, sid *session.ID, limit int) ([]event.LearningEvent, error)
	// ListAll returns every event for identity, oldest first
	// (occurred_at ASC). Used by internal/application/learnermodel to
	// fold an identity's whole history — the per-event weakness window
	// scan and `jlp rebuild-model`'s full replay both need the complete,
	// chronologically ordered stream, not just the most recent slice
	// ListRecent bounds by limit.
	ListAll(ctx context.Context, identity learner.IdentityID) ([]event.LearningEvent, error)
}
