package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// PriorityRepository persists the teaching planner's output:
// learner_priorities, PRIMARY KEY (identity_id, subject_type, subject) —
// see the 00010 migration.
type PriorityRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewPriorityRepository(pool *pgxpool.Pool) *PriorityRepository {
	return &PriorityRepository{pool: pool, q: sqlcgen.New(pool)}
}

// ReplaceAll atomically swaps identity's whole priority list — delete,
// then insert every row of ps — in one transaction, the same WithTx
// pattern GrammarRepository.UpsertConcepts and
// FeedbackRepository.InsertFeedback use: a Recompute that fails partway
// through never leaves identity with a stale row mixed in with fresh
// ones, nor wholly empty because a later insert failed after the delete
// already committed.
func (r *PriorityRepository) ReplaceAll(ctx context.Context, identity learner.IdentityID, ps []storage.Priority) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	qtx := r.q.WithTx(tx)
	if err := qtx.DeletePriorities(ctx, string(identity)); err != nil {
		return fmt.Errorf("delete priorities: %w", err)
	}
	for _, p := range ps {
		if err := qtx.InsertPriority(ctx, sqlcgen.InsertPriorityParams{
			IdentityID:  string(identity),
			SubjectType: p.SubjectType,
			Subject:     p.Subject,
			Score:       p.Score,
			Reason:      p.Reason,
			UpdatedAt:   pgtype.Timestamptz{Time: p.UpdatedAt, Valid: true},
		}); err != nil {
			return fmt.Errorf("insert priority %s/%s: %w", p.SubjectType, p.Subject, err)
		}
	}
	return tx.Commit(ctx)
}

func (r *PriorityRepository) Top(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.Priority, error) {
	rows, err := r.q.TopPriorities(ctx, sqlcgen.TopPrioritiesParams{
		IdentityID: string(identity),
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.Priority, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.Priority{
			IdentityID:  learner.IdentityID(row.IdentityID),
			SubjectType: row.SubjectType,
			Subject:     row.Subject,
			Score:       row.Score,
			Reason:      row.Reason,
			UpdatedAt:   row.UpdatedAt.Time,
		})
	}
	return out, nil
}
