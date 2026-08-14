package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type IdentityRepository interface {
	Upsert(ctx context.Context, id learner.Identity) error
	Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error)
	// ListIdentities returns every known identity, for callers — like
	// `jlp rebuild-model` — that need to act on all of them rather than
	// one at a time.
	ListIdentities(ctx context.Context) ([]learner.Identity, error)
}
