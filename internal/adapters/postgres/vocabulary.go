package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// VocabularyRepository persists the learner's personal vocabulary:
// vocabulary_items and vocabulary_events — see the 00011 migration and
// storage.VocabularyRepository's doc comment for the idempotency
// contract UpsertOnLookup implements.
type VocabularyRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewVocabularyRepository(pool *pgxpool.Pool) *VocabularyRepository {
	return &VocabularyRepository{pool: pool, q: sqlcgen.New(pool)}
}

// UpsertOnLookup runs entirely inside one transaction: when
// clientEventID is set, it first checks vocabulary_events for a row
// already carrying it (the partial UNIQUE(identity_id,
// client_event_id) index) — a match means this is a retried call, so
// it fetches and returns the existing item, duplicate=true, WITHOUT
// touching vocabulary_items or inserting a second event. Otherwise it
// upserts the item (create, or increment Lookups on a repeat lookup of
// the same expression) and appends the vocabulary_events row.
func (r *VocabularyRepository) UpsertOnLookup(ctx context.Context, identity learner.IdentityID, expression, reading, meaning, source, example string, kind vocabulary.Kind, clientEventID string, at time.Time) (vocabulary.Item, bool, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded
	qtx := r.q.WithTx(tx)

	if clientEventID != "" {
		itemID, err := qtx.GetVocabularyEventItemByClientID(ctx, sqlcgen.GetVocabularyEventItemByClientIDParams{
			IdentityID:    string(identity),
			ClientEventID: pgtype.Text{String: clientEventID, Valid: true},
		})
		switch {
		case err == nil:
			row, err := qtx.GetVocabularyItem(ctx, itemID)
			if err != nil {
				return vocabulary.Item{}, false, fmt.Errorf("vocabulary: load item for duplicate client event %q: %w", clientEventID, err)
			}
			if err := tx.Commit(ctx); err != nil {
				return vocabulary.Item{}, false, err
			}
			return fromVocabularyItemRow(row), true, nil
		case !errors.Is(err, pgx.ErrNoRows):
			return vocabulary.Item{}, false, fmt.Errorf("vocabulary: check client event %q: %w", clientEventID, err)
		}
		// pgx.ErrNoRows: no prior row for this client event — fall through
		// and record it for the first time below.
	}

	id, err := uuid.NewRandom()
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	itemIDPg := pgtype.UUID{Bytes: id, Valid: true}
	occurredAt := pgtype.Timestamptz{Time: at, Valid: true}

	row, err := qtx.UpsertVocabularyItemOnLookup(ctx, sqlcgen.UpsertVocabularyItemOnLookupParams{
		ID:         itemIDPg,
		IdentityID: string(identity),
		Expression: expression,
		Reading:    reading,
		Meaning:    meaning,
		Kind:       string(kind),
		Source:     source,
		FirstSeen:  occurredAt,
	})
	if err != nil {
		return vocabulary.Item{}, false, fmt.Errorf("vocabulary: upsert item: %w", err)
	}

	payload, err := json.Marshal(map[string]any{
		"reading": reading,
		"meaning": meaning,
		"source":  source,
		"example": example,
	})
	if err != nil {
		return vocabulary.Item{}, false, fmt.Errorf("vocabulary: marshal event payload: %w", err)
	}
	eventID, err := uuid.NewRandom()
	if err != nil {
		return vocabulary.Item{}, false, err
	}
	var clientEventPg pgtype.Text
	if clientEventID != "" {
		clientEventPg = pgtype.Text{String: clientEventID, Valid: true}
	}
	if err := qtx.InsertVocabularyEvent(ctx, sqlcgen.InsertVocabularyEventParams{
		ID:            pgtype.UUID{Bytes: eventID, Valid: true},
		IdentityID:    string(identity),
		ItemID:        row.ID,
		Type:          "vocabulary.lookup",
		Payload:       payload,
		ClientEventID: clientEventPg,
		OccurredAt:    occurredAt,
	}); err != nil {
		return vocabulary.Item{}, false, fmt.Errorf("vocabulary: insert event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return vocabulary.Item{}, false, err
	}
	return fromVocabularyItemRow(row), false, nil
}

func (r *VocabularyRepository) RecordProduction(ctx context.Context, identity learner.IdentityID, itemID string, successful bool, at time.Time) error {
	id, err := parseUUID(itemID)
	if err != nil {
		return fmt.Errorf("vocabulary: item id: %w", err)
	}
	return r.q.RecordVocabularyProduction(ctx, sqlcgen.RecordVocabularyProductionParams{
		Successful: successful,
		At:         pgtype.Timestamptz{Time: at, Valid: true},
		ID:         id,
		IdentityID: string(identity),
	})
}

func (r *VocabularyRepository) List(ctx context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	rows, err := r.q.ListVocabularyItems(ctx, sqlcgen.ListVocabularyItemsParams{
		IdentityID: string(identity),
		Filter:     filter,
	})
	if err != nil {
		return nil, err
	}
	out := make([]vocabulary.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromVocabularyItemRow(row))
	}
	return out, nil
}

// ListActivationCandidates returns identity's "activate"-filter items,
// ranked by Lookups DESC and capped at limit — see
// storage.VocabularyRepository.ListActivationCandidates' doc comment
// for why this is a dedicated query (ORDER BY + LIMIT pushed into SQL)
// rather than a Go-side sort/slice over List's output. limit <= 0 is
// clamped to 0 before reaching SQL, where NULLIF(0, 0) = NULL makes
// LIMIT NULL — postgres' spelling of "no limit" — since a negative
// LIMIT argument is itself a SQL error.
func (r *VocabularyRepository) ListActivationCandidates(ctx context.Context, identity learner.IdentityID, limit int) ([]vocabulary.Item, error) {
	if limit < 0 {
		limit = 0
	}
	rows, err := r.q.ListVocabularyActivationCandidates(ctx, sqlcgen.ListVocabularyActivationCandidatesParams{
		IdentityID: string(identity),
		LimitCount: int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]vocabulary.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromVocabularyItemRow(row))
	}
	return out, nil
}

// SeedBank inserts entries as expression-bank baseline items, all in
// one transaction (mirroring GrammarRepository.UpsertConcepts' shape),
// doing nothing per-entry when (identity, expression) already has a
// row — see storage.VocabularyRepository.SeedBank's doc comment for the
// full idempotency contract this implements via
// InsertVocabularyItemIfAbsent's ON CONFLICT DO NOTHING.
func (r *VocabularyRepository) SeedBank(ctx context.Context, identity learner.IdentityID, entries []vocabulary.BankEntry, at time.Time) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded
	qtx := r.q.WithTx(tx)

	occurredAt := pgtype.Timestamptz{Time: at, Valid: true}
	for _, e := range entries {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		if err := qtx.InsertVocabularyItemIfAbsent(ctx, sqlcgen.InsertVocabularyItemIfAbsentParams{
			ID:         pgtype.UUID{Bytes: id, Valid: true},
			IdentityID: string(identity),
			Expression: e.Expression,
			Reading:    e.Reading,
			Meaning:    e.Meaning,
			Kind:       string(e.Kind),
			Source:     "expression bank",
			FirstSeen:  occurredAt,
		}); err != nil {
			return fmt.Errorf("vocabulary: seed bank entry %q: %w", e.Expression, err)
		}
	}

	return tx.Commit(ctx)
}

func (r *VocabularyRepository) AllExpressions(ctx context.Context, identity learner.IdentityID) (map[string]string, error) {
	rows, err := r.q.ListVocabularyExpressions(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		out[row.Expression] = uuid.UUID(row.ID.Bytes).String()
	}
	return out, nil
}

// ensure the interface is satisfied at compile time.
var _ storage.VocabularyRepository = (*VocabularyRepository)(nil)

func fromVocabularyItemRow(row sqlcgen.VocabularyItem) vocabulary.Item {
	return vocabulary.Item{
		ID:                    uuid.UUID(row.ID.Bytes).String(),
		IdentityID:            learner.IdentityID(row.IdentityID),
		Expression:            row.Expression,
		Reading:               row.Reading,
		Meaning:               row.Meaning,
		Kind:                  vocabulary.Kind(row.Kind),
		JLPTLevel:             int(row.JlptLevel),
		Source:                row.Source,
		Lookups:               int(row.Lookups),
		Productions:           int(row.Productions),
		SuccessfulProductions: int(row.SuccessfulProductions),
		FirstSeen:             row.FirstSeen.Time,
		LastEvent:             row.LastEvent.Time,
	}
}
