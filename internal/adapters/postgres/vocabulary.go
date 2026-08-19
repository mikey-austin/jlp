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

// ListPage is List's paged form — see
// storage.VocabularyRepository.ListPage and migration 00028.
//
// It asks the database for limit+1 rows and returns at most limit. That
// extra row is how "is there a next page" is answered without a second
// COUNT query, and it is why the last page correctly reports no cursor
// rather than offering a link to an empty page.
func (r *VocabularyRepository) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	if limit <= 0 {
		return nil, storage.VocabularyCursor{}, nil
	}
	params := sqlcgen.ListVocabularyItemsPageParams{
		IdentityID: string(identity),
		Filter:     filter,
		PageSize:   int32(limit + 1),
	}
	if !cursor.Zero() {
		id, err := parseUUID(cursor.ID)
		if err != nil {
			// A cursor this process did not mint. Start from the
			// beginning rather than erroring: the worst case is the
			// learner seeing page one, not a broken page.
			return r.ListPage(ctx, identity, filter, storage.VocabularyCursor{}, limit)
		}
		params.CursorLastEvent = pgtype.Timestamptz{Time: cursor.LastEvent, Valid: true}
		params.CursorID = id
	}

	rows, err := r.q.ListVocabularyItemsPage(ctx, params)
	if err != nil {
		return nil, storage.VocabularyCursor{}, err
	}

	var next storage.VocabularyCursor
	if len(rows) > limit {
		// The probe row is not part of this page; it only proves another
		// page exists. The cursor is the LAST row of the page itself.
		rows = rows[:limit]
		last := rows[len(rows)-1]
		next = storage.VocabularyCursor{
			LastEvent: last.LastEvent.Time,
			ID:        uuid.UUID(last.ID.Bytes).String(),
		}
	}

	out := make([]vocabulary.Item, 0, len(rows))
	for _, row := range rows {
		out = append(out, fromVocabularyItemRow(row))
	}
	return out, next, nil
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

// ListRecentUnpracticed is 練習's first-choice drill source — see the
// port's doc comment and db/queries/vocabulary.sql for why recency
// leads. Bounded and ordered in SQL, like ListActivationCandidates
// above, so it stays one indexed query as a learner's vocabulary grows.
func (r *VocabularyRepository) ListRecentUnpracticed(ctx context.Context, identity learner.IdentityID, addedSince time.Time, limit int) ([]vocabulary.Item, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.q.ListRecentUnpracticedVocabulary(ctx, sqlcgen.ListRecentUnpracticedVocabularyParams{
		IdentityID: string(identity),
		AddedSince: pgtype.Timestamptz{Time: addedSince, Valid: true},
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

// GetByExpressions is List's bounded sibling (see
// storage.VocabularyRepository.GetByExpressions' doc comment): a
// dedicated indexed query rather than List(filter="") + a Go-side
// filter, so resolving a small, caller-supplied set of expressions
// stays cheap regardless of how large identity's vocabulary grows. An
// empty expressions returns an empty result without a query round trip.
func (r *VocabularyRepository) GetByExpressions(ctx context.Context, identity learner.IdentityID, expressions []string) ([]vocabulary.Item, error) {
	if len(expressions) == 0 {
		return nil, nil
	}
	rows, err := r.q.GetVocabularyItemsByExpressions(ctx, sqlcgen.GetVocabularyItemsByExpressionsParams{
		IdentityID:  string(identity),
		Expressions: expressions,
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

// LatestExamples implements storage.VocabularyRepository.LatestExamples
// — one query for the whole set, like GetByIDs above.
func (r *VocabularyRepository) LatestExamples(ctx context.Context, identity learner.IdentityID, ids []string) (map[string]string, error) {
	parsed := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		if u, err := parseUUID(id); err == nil {
			parsed = append(parsed, u)
		}
	}
	if len(parsed) == 0 {
		return nil, nil
	}
	rows, err := r.q.LatestVocabularyExamples(ctx, sqlcgen.LatestVocabularyExamplesParams{
		IdentityID: string(identity),
		Ids:        parsed,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(rows))
	for _, row := range rows {
		// payload->>'example' is jsonb text, which sqlc types as
		// interface{}; anything that is not a string is a payload we did
		// not write and have no use for.
		example, ok := row.Example.(string)
		if !ok || example == "" {
			continue
		}
		out[uuid.UUID(row.ItemID.Bytes).String()] = example
	}
	return out, nil
}

// GetByIDs is GetByExpressions' sibling for callers holding an ID —
// see the port's doc comment. An empty ids returns an empty result
// without a query round trip, exactly as GetByExpressions does.
func (r *VocabularyRepository) GetByIDs(ctx context.Context, identity learner.IdentityID, ids []string) ([]vocabulary.Item, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	parsed := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := parseUUID(id)
		if err != nil {
			// A subject that is not a UUID is not one of our vocabulary
			// rows — skipped rather than failed, because the caller is
			// scanning a mixed list of due subjects (concepts are slugs).
			continue
		}
		parsed = append(parsed, u)
	}
	if len(parsed) == 0 {
		return nil, nil
	}
	rows, err := r.q.GetVocabularyItemsByIDs(ctx, sqlcgen.GetVocabularyItemsByIDsParams{
		IdentityID: string(identity),
		Ids:        parsed,
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

// BulkUpsertWords upserts words by (identity_id, expression) in ONE
// transaction — see storage.VocabularyRepository.BulkUpsertWords' doc
// comment for the exact sparse-merge/counters-untouched contract
// UpsertVocabularyWord's ON CONFLICT clause implements. Unlike
// SeedBank's ON CONFLICT DO NOTHING, a conflict here DOES update the
// row (that's the whole point of a re-sync), just without ever
// touching lookups/productions/successful_productions.
func (r *VocabularyRepository) BulkUpsertWords(ctx context.Context, identity learner.IdentityID, words []storage.WordInput, at time.Time) (int, error) {
	if len(words) == 0 {
		return 0, nil
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded
	qtx := r.q.WithTx(tx)

	occurredAt := pgtype.Timestamptz{Time: at, Valid: true}
	for _, w := range words {
		id, err := uuid.NewRandom()
		if err != nil {
			return 0, err
		}
		tags := w.Tags
		if tags == nil {
			tags = []string{}
		}
		tagsJSON, err := json.Marshal(tags)
		if err != nil {
			return 0, fmt.Errorf("vocabulary: marshal tags for %q: %w", w.Expression, err)
		}

		if err := qtx.UpsertVocabularyWord(ctx, sqlcgen.UpsertVocabularyWordParams{
			ID:         pgtype.UUID{Bytes: id, Valid: true},
			IdentityID: string(identity),
			Expression: w.Expression,
			Reading:    w.Reading,
			Meaning:    w.Meaning,
			MeaningEn:  w.MeaningEN,
			Kind:       string(vocabulary.KindWord),
			JlptLevel:  int32(w.JLPTLevel),
			Source:     w.Source,
			Tags:       tagsJSON,
			FirstSeen:  occurredAt,
		}); err != nil {
			return 0, fmt.Errorf("vocabulary: bulk upsert word %q: %w", w.Expression, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return len(words), nil
}

// SoftDelete marks the item deleted — see
// storage.VocabularyRepository.SoftDelete for the contract. The rows-
// affected count is the whole authorization answer: SoftDeleteVocabulary
// Item's WHERE carries identity_id, so zero rows means "no such item for
// THIS identity", whether the id belongs to someone else or to nobody,
// and both come back as ErrNotFound. Mirrors SessionRepository.
// SoftDelete exactly, including mapping a malformed id to ErrNotFound
// rather than a distinguishable parse error.
func (r *VocabularyRepository) SoftDelete(ctx context.Context, identity learner.IdentityID, itemID string, at time.Time) error {
	id, err := parseUUID(itemID)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.SoftDeleteVocabularyItem(ctx, sqlcgen.SoftDeleteVocabularyItemParams{
		ID:         id,
		IdentityID: string(identity),
		At:         pgtype.Timestamptz{Time: at, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// Restore clears the deletion mark — see
// storage.VocabularyRepository.Restore.
func (r *VocabularyRepository) Restore(ctx context.Context, identity learner.IdentityID, itemID string) error {
	id, err := parseUUID(itemID)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.RestoreVocabularyItem(ctx, sqlcgen.RestoreVocabularyItemParams{
		ID:         id,
		IdentityID: string(identity),
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ensure the interface is satisfied at compile time.
var _ storage.VocabularyRepository = (*VocabularyRepository)(nil)

func fromVocabularyItemRow(row sqlcgen.VocabularyItem) vocabulary.Item {
	var tags []string
	if len(row.Tags) > 0 {
		// A malformed tags payload can't happen from this adapter's own
		// writes (BulkUpsertWords always marshals a valid []string), but
		// tolerate it defensively rather than propagating a read error —
		// tags is display-only, unlike the required fields above.
		_ = json.Unmarshal(row.Tags, &tags)
	}
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
		MeaningEN:             row.MeaningEn,
		Tags:                  tags,
	}
}
