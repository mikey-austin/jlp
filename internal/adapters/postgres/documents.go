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
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type DocumentRepository struct {
	pool *pgxpool.Pool
	q    *sqlcgen.Queries
}

func NewDocumentRepository(pool *pgxpool.Pool) *DocumentRepository {
	return &DocumentRepository{pool: pool, q: sqlcgen.New(pool)}
}

// GetOrCreateForSession returns the session's single document, creating an
// empty one (version 1) the first time the session's workspace is opened.
func (r *DocumentRepository) GetOrCreateForSession(ctx context.Context, identity learner.IdentityID, sid session.ID) (writing.Document, error) {
	pgSID, err := parseUUID(string(sid))
	if err != nil {
		return writing.Document{}, fmt.Errorf("session id: %w", err)
	}

	row, err := r.q.GetDocumentBySession(ctx, sqlcgen.GetDocumentBySessionParams{SessionID: pgSID, IdentityID: string(identity)})
	if err == nil {
		return fromDocumentRow(row), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return writing.Document{}, err
	}

	id, err := uuid.NewRandom()
	if err != nil {
		return writing.Document{}, err
	}
	row, err = r.q.InsertDocument(ctx, sqlcgen.InsertDocumentParams{
		ID:         pgtype.UUID{Bytes: id, Valid: true},
		SessionID:  pgSID,
		IdentityID: string(identity),
		Content:    "",
		Version:    1,
		UpdatedAt:  pgtype.Timestamptz{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		return writing.Document{}, err
	}
	return fromDocumentRow(row), nil
}

// Save persists new content, incrementing Version and appending a
// document_versions row in one transaction so a save can never bump the
// version without also recording the version's content (or vice versa).
func (r *DocumentRepository) Save(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, content string) (writing.Document, error) {
	pgID, err := parseUUID(string(id))
	if err != nil {
		return writing.Document{}, fmt.Errorf("document id: %w", err)
	}

	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return writing.Document{}, err
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	qtx := r.q.WithTx(tx)
	updated, err := qtx.UpdateDocumentContent(ctx, sqlcgen.UpdateDocumentContentParams{
		ID:         pgID,
		IdentityID: string(identity),
		Content:    content,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return writing.Document{}, storage.ErrNotFound
		}
		return writing.Document{}, err
	}

	if err := qtx.InsertDocumentVersion(ctx, sqlcgen.InsertDocumentVersionParams{
		DocumentID: updated.ID,
		Version:    updated.Version,
		Content:    updated.Content,
		CreatedAt:  updated.UpdatedAt,
	}); err != nil {
		return writing.Document{}, err
	}

	if err := tx.Commit(ctx); err != nil {
		return writing.Document{}, err
	}
	return fromDocumentRow(updated), nil
}

func (r *DocumentRepository) Get(ctx context.Context, identity learner.IdentityID, id writing.DocumentID) (writing.Document, error) {
	pgID, err := parseUUID(string(id))
	if err != nil {
		return writing.Document{}, fmt.Errorf("document id: %w", err)
	}
	row, err := r.q.GetDocument(ctx, sqlcgen.GetDocumentParams{ID: pgID, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return writing.Document{}, storage.ErrNotFound
		}
		return writing.Document{}, err
	}
	return fromDocumentRow(row), nil
}

// ListVersions returns up to limit historical versions, newest first.
// document_versions rows don't carry session_id, so returned Documents
// leave SessionID zero — callers already know it from the document itself.
func (r *DocumentRepository) ListVersions(ctx context.Context, identity learner.IdentityID, id writing.DocumentID, limit int) ([]writing.Document, error) {
	pgID, err := parseUUID(string(id))
	if err != nil {
		return nil, fmt.Errorf("document id: %w", err)
	}
	rows, err := r.q.ListDocumentVersions(ctx, sqlcgen.ListDocumentVersionsParams{
		DocumentID: pgID,
		IdentityID: string(identity),
		Limit:      int32(limit),
	})
	if err != nil {
		return nil, err
	}
	out := make([]writing.Document, 0, len(rows))
	for _, row := range rows {
		out = append(out, writing.Document{
			ID:         id,
			IdentityID: identity,
			Content:    row.Content,
			Version:    int(row.Version),
			UpdatedAt:  row.CreatedAt.Time,
		})
	}
	return out, nil
}

// parseUUID converts a canonical UUID string (session, document, etc. IDs
// are all google/uuid strings at the application boundary) to the pgtype
// sqlc generates for postgres `uuid` columns.
func parseUUID(s string) (pgtype.UUID, error) {
	u, err := uuid.Parse(s)
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

func fromDocumentRow(row sqlcgen.Document) writing.Document {
	return writing.Document{
		ID:         writing.DocumentID(uuid.UUID(row.ID.Bytes).String()),
		SessionID:  session.ID(uuid.UUID(row.SessionID.Bytes).String()),
		IdentityID: learner.IdentityID(row.IdentityID),
		Content:    row.Content,
		Version:    int(row.Version),
		UpdatedAt:  row.UpdatedAt.Time,
	}
}
