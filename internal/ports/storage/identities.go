package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type IdentityRepository interface {
	Upsert(ctx context.Context, id learner.Identity) error
	Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error)
}
