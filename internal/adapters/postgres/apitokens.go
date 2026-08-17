package postgres

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// APITokenRepository persists credentials for non-browser clients.
// Only hashes land here — see migration 00027.
type APITokenRepository struct{ q *sqlcgen.Queries }

func NewAPITokenRepository(pool *pgxpool.Pool) *APITokenRepository {
	return &APITokenRepository{q: sqlcgen.New(pool)}
}

func (r *APITokenRepository) Insert(ctx context.Context, id string, identity learner.IdentityID, name, hash string, scopes []string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.q.InsertAPIToken(ctx, sqlcgen.InsertAPITokenParams{
		ID:         uid,
		IdentityID: string(identity),
		Name:       name,
		TokenHash:  hash,
		Scopes:     strings.Join(scopes, " "),
	})
}

func (r *APITokenRepository) LookupLive(ctx context.Context, hash string) (storage.APITokenGrant, error) {
	row, err := r.q.GetLiveAPITokenByHash(ctx, hash)
	if errors.Is(err, pgx.ErrNoRows) {
		// Deliberately the same error for "no such token", "revoked" and
		// "malformed": a caller — and anyone probing the endpoint —
		// learns only that these credentials do not work.
		return storage.APITokenGrant{}, storage.ErrNotFound
	}
	if err != nil {
		return storage.APITokenGrant{}, err
	}
	return storage.APITokenGrant{
		ID:       uuid.UUID(row.ID.Bytes).String(),
		Identity: learner.IdentityID(row.IdentityID),
		Scopes:   splitScopes(row.Scopes),
	}, nil
}

func (r *APITokenRepository) TouchLastUsed(ctx context.Context, id string) error {
	uid, err := parseUUID(id)
	if err != nil {
		return err
	}
	return r.q.TouchAPITokenLastUsed(ctx, uid)
}

func (r *APITokenRepository) List(ctx context.Context, identity learner.IdentityID) ([]storage.APIToken, error) {
	rows, err := r.q.ListAPITokens(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.APIToken, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.APIToken{
			ID:         uuid.UUID(row.ID.Bytes).String(),
			Name:       row.Name,
			Scopes:     splitScopes(row.Scopes),
			CreatedAt:  row.CreatedAt.Time,
			LastUsedAt: nullableTime(row.LastUsedAt),
			RevokedAt:  nullableTime(row.RevokedAt),
		})
	}
	return out, nil
}

func (r *APITokenRepository) Revoke(ctx context.Context, id string, identity learner.IdentityID) (bool, error) {
	uid, err := parseUUID(id)
	if err != nil {
		// A malformed id revokes nothing rather than erroring: the caller
		// is a form post, and "that is not one of your tokens" is the
		// same answer either way.
		return false, nil
	}
	n, err := r.q.RevokeAPIToken(ctx, sqlcgen.RevokeAPITokenParams{ID: uid, IdentityID: string(identity)})
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func splitScopes(s string) []string {
	f := strings.Fields(s)
	if len(f) == 0 {
		return nil
	}
	return f
}

func nullableTime(t pgtype.Timestamptz) *time.Time {
	if !t.Valid {
		return nil
	}
	v := t.Time
	return &v
}
