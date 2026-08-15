package postgres

import (
	"context"
	"errors"
	"fmt"

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

// ApprovedForExport returns every "approved" card for identity, oldest
// first.
func (r *AnkiCardRepository) ApprovedForExport(ctx context.Context, identity learner.IdentityID) ([]storage.AnkiCard, error) {
	rows, err := r.q.ApprovedAnkiCardsForExport(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	cards := make([]storage.AnkiCard, 0, len(rows))
	for _, row := range rows {
		cards = append(cards, fromAnkiCardRow(row))
	}
	return cards, nil
}

// MarkExported flips exactly the given ids to "exported", scoped to
// identity and restricted to cards still "approved" (see
// db/queries/anki.sql's MarkAnkiCardsExported doc comment: calling it
// twice with the same ids is a safe no-op the second time). An empty
// ids is a no-op — no query is issued.
func (r *AnkiCardRepository) MarkExported(ctx context.Context, identity learner.IdentityID, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	uuids := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		u, err := parseUUID(id)
		if err != nil {
			return fmt.Errorf("anki card id: %w", err)
		}
		uuids = append(uuids, u)
	}
	return r.q.MarkAnkiCardsExported(ctx, sqlcgen.MarkAnkiCardsExportedParams{
		IdentityID: string(identity),
		Ids:        uuids,
	})
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
