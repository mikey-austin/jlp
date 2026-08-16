package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// RetrievalRepository persists spaced-retrieval schedules (PRD §54):
// retrieval_items, PRIMARY KEY (identity_id, subject_type, subject) —
// see the 00024 migration and db/queries/retrieval.sql for the ON
// CONFLICT replace semantics Upsert relies on, and for the
// LIMIT NULLIF(..., 0) "0 means unlimited" convention Due/List share
// with VocabularyRepository.ListActivationCandidates.
type RetrievalRepository struct{ q *sqlcgen.Queries }

func NewRetrievalRepository(pool *pgxpool.Pool) *RetrievalRepository {
	return &RetrievalRepository{q: sqlcgen.New(pool)}
}

func (r *RetrievalRepository) Upsert(ctx context.Context, it storage.RetrievalItem) error {
	return r.q.UpsertRetrievalItem(ctx, sqlcgen.UpsertRetrievalItemParams{
		IdentityID:      string(it.IdentityID),
		SubjectType:     it.SubjectType,
		Subject:         it.Subject,
		Successes:       int32(it.Successes),
		Failures:        int32(it.Failures),
		LastSeen:        pgtype.Timestamptz{Time: it.LastSeen, Valid: true},
		DueAt:           pgtype.Timestamptz{Time: it.DueAt, Valid: true},
		IntervalSeconds: int64(it.Interval / time.Second),
		Confidence:      it.Confidence,
	})
}

// Get is identity-scoped via the WHERE identity_id = $1 clause itself
// (not a post-hoc filter) — a subject that exists but belongs to a
// different identity is indistinguishable, at the SQL level, from one
// that was never scheduled at all, so both miss with ErrNotFound alike.
func (r *RetrievalRepository) Get(ctx context.Context, identity learner.IdentityID, subjectType, subject string) (storage.RetrievalItem, error) {
	row, err := r.q.GetRetrievalItem(ctx, sqlcgen.GetRetrievalItemParams{
		IdentityID:  string(identity),
		SubjectType: subjectType,
		Subject:     subject,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.RetrievalItem{}, storage.ErrNotFound
		}
		return storage.RetrievalItem{}, err
	}
	return fromRetrievalItemRow(row), nil
}

func (r *RetrievalRepository) Due(ctx context.Context, identity learner.IdentityID, at time.Time, limit int) ([]storage.RetrievalItem, error) {
	if limit < 0 {
		limit = 0
	}
	rows, err := r.q.DueRetrievalItems(ctx, sqlcgen.DueRetrievalItemsParams{
		IdentityID: string(identity),
		DueAt:      pgtype.Timestamptz{Time: at, Valid: true},
		LimitCount: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return fromRetrievalItemRows(rows), nil
}

func (r *RetrievalRepository) List(ctx context.Context, identity learner.IdentityID, limit int) ([]storage.RetrievalItem, error) {
	if limit < 0 {
		limit = 0
	}
	rows, err := r.q.ListRetrievalItems(ctx, sqlcgen.ListRetrievalItemsParams{
		IdentityID: string(identity),
		LimitCount: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	return fromRetrievalItemRows(rows), nil
}

func fromRetrievalItemRows(rows []sqlcgen.RetrievalItem) []storage.RetrievalItem {
	out := make([]storage.RetrievalItem, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromRetrievalItemRow(row))
	}
	return out
}

func fromRetrievalItemRow(row sqlcgen.RetrievalItem) storage.RetrievalItem {
	return storage.RetrievalItem{
		IdentityID:  learner.IdentityID(row.IdentityID),
		SubjectType: row.SubjectType,
		Subject:     row.Subject,
		Successes:   int(row.Successes),
		Failures:    int(row.Failures),
		LastSeen:    row.LastSeen.Time,
		DueAt:       row.DueAt.Time,
		Interval:    time.Duration(row.IntervalSeconds) * time.Second,
		Confidence:  row.Confidence,
	}
}
