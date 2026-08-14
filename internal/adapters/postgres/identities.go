package postgres

import (
	"context"
	"encoding/json"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

type IdentityRepository struct{ q *sqlcgen.Queries }

func NewIdentityRepository(pool *pgxpool.Pool) *IdentityRepository {
	return &IdentityRepository{q: sqlcgen.New(pool)}
}

func (r *IdentityRepository) Upsert(ctx context.Context, id learner.Identity) error {
	attrs, err := json.Marshal(id.Attributes)
	if err != nil {
		return err
	}
	return r.q.UpsertIdentity(ctx, sqlcgen.UpsertIdentityParams{
		ID: string(id.ID), DisplayName: id.DisplayName, Attributes: attrs,
	})
}

func (r *IdentityRepository) Get(ctx context.Context, id learner.IdentityID) (learner.Identity, error) {
	row, err := r.q.GetIdentity(ctx, string(id))
	if err != nil {
		return learner.Identity{}, err
	}
	var attrs map[string]string
	_ = json.Unmarshal(row.Attributes, &attrs)
	return learner.Identity{ID: learner.IdentityID(row.ID), DisplayName: row.DisplayName, Attributes: attrs}, nil
}
