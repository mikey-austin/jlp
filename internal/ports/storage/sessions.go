package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

type SessionRepository interface {
	Create(ctx context.Context, s session.Session) error
	Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error)
	List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error)
}
