package postgres

import (
	"context"
	"encoding/json"
	"fmt"

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
	if err := json.Unmarshal(row.Attributes, &attrs); err != nil {
		return learner.Identity{}, fmt.Errorf("identity %s attributes: %w", id, err)
	}
	return learner.Identity{ID: learner.IdentityID(row.ID), DisplayName: row.DisplayName, Attributes: attrs}, nil
}

// ListIdentities returns every identity, for callers — like
// `jlp rebuild-model` — that need to act on all of them.
func (r *IdentityRepository) ListIdentities(ctx context.Context) ([]learner.Identity, error) {
	rows, err := r.q.ListIdentities(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]learner.Identity, 0, len(rows))
	for _, row := range rows {
		var attrs map[string]string
		if err := json.Unmarshal(row.Attributes, &attrs); err != nil {
			return nil, fmt.Errorf("identity %s attributes: %w", row.ID, err)
		}
		out = append(out, learner.Identity{ID: learner.IdentityID(row.ID), DisplayName: row.DisplayName, Attributes: attrs})
	}
	return out, nil
}
