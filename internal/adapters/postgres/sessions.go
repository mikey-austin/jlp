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
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type SessionRepository struct{ q *sqlcgen.Queries }

func NewSessionRepository(pool *pgxpool.Pool) *SessionRepository {
	return &SessionRepository{q: sqlcgen.New(pool)}
}

// Note: sqlc infers the sessions.id column (postgres `uuid`) as
// pgtype.UUID, not the domain's `session.ID` string type. This repo
// converts at the boundary via google/uuid, which the application layer
// already uses to generate session IDs as canonical UUID strings.

func (r *SessionRepository) Create(ctx context.Context, s session.Session) error {
	id, err := toPgUUID(s.ID)
	if err != nil {
		return fmt.Errorf("session id: %w", err)
	}
	profile, err := json.Marshal(s.Profile)
	if err != nil {
		return err
	}
	return r.q.CreateSession(ctx, sqlcgen.CreateSessionParams{
		ID:         id,
		IdentityID: string(s.IdentityID),
		Title:      s.Title,
		Purpose:    s.Purpose,
		Profile:    profile,
		CreatedAt:  pgtype.Timestamptz{Time: s.CreatedAt, Valid: true},
		UpdatedAt:  pgtype.Timestamptz{Time: s.UpdatedAt, Valid: true},
	})
}

func (r *SessionRepository) Get(ctx context.Context, identity learner.IdentityID, id session.ID) (session.Session, error) {
	pgID, err := toPgUUID(id)
	if err != nil {
		return session.Session{}, fmt.Errorf("session id: %w", err)
	}
	row, err := r.q.GetSession(ctx, sqlcgen.GetSessionParams{ID: pgID, IdentityID: string(identity)})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return session.Session{}, storage.ErrNotFound
		}
		return session.Session{}, err
	}
	return fromSessionRow(row)
}

func (r *SessionRepository) List(ctx context.Context, identity learner.IdentityID) ([]session.Session, error) {
	rows, err := r.q.ListSessions(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]session.Session, 0, len(rows))
	for _, row := range rows {
		s, err := fromSessionRow(row)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// SoftDelete marks the session deleted — see
// storage.SessionRepository.SoftDelete for the contract. The rows-
// affected count is the whole authorization answer: SoftDeleteSession's
// WHERE carries identity_id, so zero rows means "no such session for
// THIS identity", whether the id belongs to someone else or to nobody,
// and both come back as ErrNotFound. A malformed id maps to ErrNotFound
// too rather than a parse error, so a caller probing with junk learns
// nothing an unknown-but-valid id would not also tell them.
func (r *SessionRepository) SoftDelete(ctx context.Context, identity learner.IdentityID, id session.ID, at time.Time) error {
	pgID, err := toPgUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.SoftDeleteSession(ctx, sqlcgen.SoftDeleteSessionParams{
		ID:         pgID,
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
// storage.SessionRepository.Restore. Same identity-scoping and same
// zero-rows-is-ErrNotFound mapping as SoftDelete above.
func (r *SessionRepository) Restore(ctx context.Context, identity learner.IdentityID, id session.ID) error {
	pgID, err := toPgUUID(id)
	if err != nil {
		return storage.ErrNotFound
	}
	rows, err := r.q.RestoreSession(ctx, sqlcgen.RestoreSessionParams{
		ID:         pgID,
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

func toPgUUID(id session.ID) (pgtype.UUID, error) {
	u, err := uuid.Parse(string(id))
	if err != nil {
		return pgtype.UUID{}, err
	}
	return pgtype.UUID{Bytes: u, Valid: true}, nil
}

func fromSessionRow(row sqlcgen.Session) (session.Session, error) {
	var profile session.Profile
	if err := json.Unmarshal(row.Profile, &profile); err != nil {
		return session.Session{}, fmt.Errorf("session %s profile: %w", uuid.UUID(row.ID.Bytes), err)
	}
	return session.Session{
		ID:         session.ID(uuid.UUID(row.ID.Bytes).String()),
		IdentityID: learner.IdentityID(row.IdentityID),
		Title:      row.Title,
		Purpose:    row.Purpose,
		Profile:    profile,
		CreatedAt:  row.CreatedAt.Time,
		UpdatedAt:  row.UpdatedAt.Time,
	}, nil
}
