package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// AnkiCardRepository persists the Anki review queue (PRD §19): the
// anki_cards table (00014 migration) and storage.AnkiCardRepository's
// doc comment for the full status-lifecycle contract.
type AnkiCardRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewAnkiCardRepository(pool *pgxpool.Pool) *AnkiCardRepository {
	return &AnkiCardRepository{pool: pool, q: sqlcgen.New(pool)}
}

// Insert persists a newly generated draft card.
func (r *AnkiCardRepository) Insert(ctx context.Context, c storage.AnkiCard) error {
	id, err := parseUUID(c.ID)
	if err != nil {
		return fmt.Errorf("anki card id: %w", err)
	}
	return r.q.InsertAnkiCard(ctx, sqlcgen.InsertAnkiCardParams{
		ID:         id,
		IdentityID: string(c.IdentityID),
		SourceType: c.SourceType,
		SourceID:   c.SourceID,
		Front:      c.Front,
		Back:       c.Back,
		Notes:      c.Notes,
		Status:     c.Status,
		CreatedAt:  pgtype.Timestamptz{Time: c.CreatedAt, Valid: true},
	})
}

// List returns identity's cards, newest first; status "" returns every
// card regardless of status (see db/queries/anki.sql's ListAnkiCards).
func (r *AnkiCardRepository) List(ctx context.Context, identity learner.IdentityID, status string) ([]storage.AnkiCard, error) {
	rows, err := r.q.ListAnkiCards(ctx, sqlcgen.ListAnkiCardsParams{IdentityID: string(identity), Status: status})
	if err != nil {
		return nil, err
	}
	cards := make([]storage.AnkiCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, fromAnkiCardRow(row))
	}
	return cards, nil
}

// UpdateStatus sets id's status, identity-scoped: a wrong identity or
// unknown id both miss with storage.ErrNotFound.
func (r *AnkiCardRepository) UpdateStatus(ctx context.Context, identity learner.IdentityID, id, status string) (storage.AnkiCard, error) {
	cardID, err := parseUUID(id)
	if err != nil {
		return storage.AnkiCard{}, fmt.Errorf("anki card id: %w", err)
	}
	row, err := r.q.UpdateAnkiCardStatus(ctx, sqlcgen.UpdateAnkiCardStatusParams{
		ID:         cardID,
		IdentityID: string(identity),
		Status:     status,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return storage.AnkiCard{}, storage.ErrNotFound
		}
		return storage.AnkiCard{}, err
	}
	return fromAnkiCardRow(row), nil
}

// TakeApprovedForExport atomically reads every "approved" card for
// identity and marks them "exported", in ONE transaction: SELECT ...
// FOR UPDATE (row-locking, serializing concurrent callers for the same
// identity) followed by an UPDATE restricted to exactly the ids that
// SELECT returned — see storage.AnkiCardRepository's doc comment for
// the full concurrency contract this implements, and
// db/queries/anki.sql's SelectApprovedAnkiCardsForUpdate/
// MarkAnkiCardsExportedByIDs doc comments for the two statements this
// runs. at is accepted (future use / clock injection) but not
// persisted — see the port's doc comment.
func (r *AnkiCardRepository) TakeApprovedForExport(ctx context.Context, identity learner.IdentityID, _ time.Time) ([]storage.AnkiCard, error) {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op once Commit has succeeded
	qtx := r.q.WithTx(tx)

	rows, err := qtx.SelectApprovedAnkiCardsForUpdate(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, tx.Commit(ctx)
	}

	ids := make([]pgtype.UUID, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if err := qtx.MarkAnkiCardsExportedByIDs(ctx, sqlcgen.MarkAnkiCardsExportedByIDsParams{
		IdentityID: string(identity),
		Ids:        ids,
	}); err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	// rows themselves still read "approved" (that's what SELECT saw
	// before the UPDATE above) — the returned cards must reflect the
	// post-commit truth instead.
	cards := make([]storage.AnkiCard, 0, len(rows))
	for _, row := range rows {
		c := fromAnkiCardRow(row)
		c.Status = "exported"
		cards = append(cards, c)
	}
	return cards, nil
}

func fromAnkiCardRow(row sqlcgen.AnkiCard) storage.AnkiCard {
	return storage.AnkiCard{
		ID:         uuid.UUID(row.ID.Bytes).String(),
		IdentityID: learner.IdentityID(row.IdentityID),
		SourceType: row.SourceType,
		SourceID:   row.SourceID,
		Front:      row.Front,
		Back:       row.Back,
		Notes:      row.Notes,
		Status:     row.Status,
		CreatedAt:  row.CreatedAt.Time,
	}
}

// ensure the interface is satisfied at compile time.
var _ storage.AnkiCardRepository = (*AnkiCardRepository)(nil)
