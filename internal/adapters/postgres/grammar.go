package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type GrammarRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewGrammarRepository(pool *pgxpool.Pool) *GrammarRepository {
	return &GrammarRepository{pool: pool, q: sqlcgen.New(pool)}
}

// UpsertConcepts loads the curated catalog (see cmd/jlp/seed.go, which
// reads data/grammar/concepts.yaml through grammar.LoadCatalog) into
// grammar_concepts in one transaction, the same WithTx pattern
// DocumentRepository.Save and FeedbackRepository.InsertFeedback use: a
// `make seed` interrupted partway through never leaves the catalog
// half-written. Reference data, so — unlike every other repository in
// this package — there is no identity to scope by.
func (r *GrammarRepository) UpsertConcepts(ctx context.Context, cs []grammar.Concept) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded

	qtx := r.q.WithTx(tx)
	for _, c := range cs {
		examples, err := json.Marshal(c.Examples)
		if err != nil {
			return fmt.Errorf("concept %q: marshal examples: %w", c.Slug, err)
		}
		related, err := json.Marshal(c.Related)
		if err != nil {
			return fmt.Errorf("concept %q: marshal related: %w", c.Slug, err)
		}
		prerequisites, err := json.Marshal(c.Prerequisites)
		if err != nil {
			return fmt.Errorf("concept %q: marshal prerequisites: %w", c.Slug, err)
		}
		if err := qtx.UpsertGrammarConcept(ctx, sqlcgen.UpsertGrammarConceptParams{
			Slug:          c.Slug,
			Name:          c.Name,
			JlptLevel:     int32(c.JLPTLevel),
			Description:   c.Description,
			Examples:      examples,
			Related:       related,
			Prerequisites: prerequisites,
		}); err != nil {
			return fmt.Errorf("concept %q: %w", c.Slug, err)
		}
	}
	return tx.Commit(ctx)
}

func (r *GrammarRepository) ListConcepts(ctx context.Context) ([]grammar.Concept, error) {
	rows, err := r.q.ListGrammarConcepts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]grammar.Concept, 0, len(rows))
	for _, row := range rows {
		c, err := fromGrammarConceptRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func (r *GrammarRepository) GetConcept(ctx context.Context, slug string) (grammar.Concept, error) {
	row, err := r.q.GetGrammarConcept(ctx, slug)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return grammar.Concept{}, storage.ErrNotFound
		}
		return grammar.Concept{}, err
	}
	return fromGrammarConceptRow(row)
}

// ConceptStats reports how identity's corrections relate to every
// concept in the catalog, including concepts identity has never been
// tagged for (Encounters 0, LastSeen zero) — see db/queries/grammar.sql
// for why the underlying query joins through a subquery scoped to
// identity rather than filtering with an outer WHERE.
func (r *GrammarRepository) ConceptStats(ctx context.Context, identity learner.IdentityID) ([]storage.ConceptStat, error) {
	rows, err := r.q.ConceptStats(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.ConceptStat, 0, len(rows))
	for _, row := range rows {
		lastSeen, err := lastSeenTime(row.LastSeen)
		if err != nil {
			return nil, fmt.Errorf("concept %q: %w", row.Slug, err)
		}
		out = append(out, storage.ConceptStat{
			Slug:       row.Slug,
			Name:       row.Name,
			JLPTLevel:  int(row.JlptLevel),
			Encounters: int(row.Encounters),
			LastSeen:   lastSeen,
		})
	}
	return out, nil
}

func (r *GrammarRepository) CorrectionsForConcept(ctx context.Context, identity learner.IdentityID, slug string, limit int) ([]storage.CorrectionRecord, error) {
	rows, err := r.q.CorrectionsForConcept(ctx, sqlcgen.CorrectionsForConceptParams{
		IdentityID:  string(identity),
		ConceptSlug: slug,
		Limit:       int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]storage.CorrectionRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.CorrectionRecord{
			ID:            uuid.UUID(row.ID.Bytes).String(),
			FeedbackID:    uuid.UUID(row.FeedbackRequestID.Bytes).String(),
			Position:      int(row.Position),
			Original:      row.Original,
			Replacement:   row.Replacement,
			Type:          row.Type,
			Severity:      row.Severity,
			ExplanationJA: row.ExplanationJa,
			ExplanationEN: row.ExplanationEn,
			Status:        row.Status,
			SessionID:     session.ID(uuid.UUID(row.SessionID.Bytes).String()),
		})
	}
	return out, nil
}

func fromGrammarConceptRow(row sqlcgen.GrammarConcept) (grammar.Concept, error) {
	c := grammar.Concept{
		Slug:        row.Slug,
		Name:        row.Name,
		JLPTLevel:   int(row.JlptLevel),
		Description: row.Description,
	}
	if err := json.Unmarshal(row.Examples, &c.Examples); err != nil {
		return grammar.Concept{}, fmt.Errorf("concept %q: unmarshal examples: %w", row.Slug, err)
	}
	if err := json.Unmarshal(row.Related, &c.Related); err != nil {
		return grammar.Concept{}, fmt.Errorf("concept %q: unmarshal related: %w", row.Slug, err)
	}
	if err := json.Unmarshal(row.Prerequisites, &c.Prerequisites); err != nil {
		return grammar.Concept{}, fmt.Errorf("concept %q: unmarshal prerequisites: %w", row.Slug, err)
	}
	return c, nil
}

// lastSeenTime unwraps ConceptStatsRow.LastSeen. sqlc types that column
// as interface{} because its postgres type inference can't resolve a
// static type through COALESCE(MAX(...), 'epoch'::timestamptz) — but
// the column is a real timestamptz at runtime, and pgx's default type
// map decodes a timestamptz into a time.Time even when the scan target
// is interface{}, so this simply asserts rather than re-deriving
// anything.
//
// 'epoch'::timestamptz (1970-01-01 00:00:00 UTC) is the SQL "never"
// sentinel the COALESCE falls back to when a concept has no matching
// corrections; it round-trips to a non-zero Go time.Time, so it's
// normalized to the actual zero value here to match
// storage.ConceptStat.LastSeen's documented "zero when never".
func lastSeenTime(v interface{}) (time.Time, error) {
	t, ok := v.(time.Time)
	if !ok {
		return time.Time{}, fmt.Errorf("last_seen: unexpected scan type %T", v)
	}
	if t.Unix() == 0 {
		return time.Time{}, nil
	}
	return t, nil
}
