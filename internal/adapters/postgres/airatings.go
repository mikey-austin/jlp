package postgres

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// AIRatingRepository persists learner ratings of AI responses. One
// rating per (ai_request_id, identity): Upsert relies on the table's
// UNIQUE constraint plus ON CONFLICT so rating the same request twice
// replaces the value instead of creating a second row.
type AIRatingRepository struct{ q *sqlcgen.Queries }

func NewAIRatingRepository(pool *pgxpool.Pool) *AIRatingRepository {
	return &AIRatingRepository{q: sqlcgen.New(pool)}
}

// Upsert writes rt only if rt.AIRequestID actually belongs to
// rt.IdentityID (verified via a join to ai_requests inside the query
// itself — see db/queries/ai_ratings.sql): an identity can never
// attach a rating to another identity's AI request just by supplying
// its id. When no such request exists for that identity, it returns
// storage.ErrNotFound, the same "not yours" signal
// FeedbackRepository.UpdateCorrectionStatus gives.
func (r *AIRatingRepository) Upsert(ctx context.Context, rt storage.AIRating) error {
	id, err := parseUUID(rt.ID)
	if err != nil {
		return fmt.Errorf("ai rating id: %w", err)
	}
	requestID, err := parseUUID(rt.AIRequestID)
	if err != nil {
		return fmt.Errorf("ai request id: %w", err)
	}
	rows, err := r.q.UpsertAIRating(ctx, sqlcgen.UpsertAIRatingParams{
		ID:          id,
		AiRequestID: requestID,
		IdentityID:  string(rt.IdentityID),
		Rating:      int32(rt.Rating),
		CreatedAt:   pgtype.Timestamptz{Time: rt.CreatedAt, Valid: true},
	})
	if err != nil {
		return err
	}
	if rows == 0 {
		return storage.ErrNotFound
	}
	return nil
}

// ForRequests returns identity's ratings for requestIDs, keyed by
// AIRequestID. Scoped by identity_id in the query itself (see
// db/queries/ai_ratings.sql), not filtered after the fact: another
// identity's rating of one of these requests is never fetched, let
// alone returned.
func (r *AIRatingRepository) ForRequests(ctx context.Context, identity learner.IdentityID, requestIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(requestIDs) == 0 {
		return out, nil
	}
	ids := make([]pgtype.UUID, 0, len(requestIDs))
	for _, s := range requestIDs {
		id, err := parseUUID(s)
		if err != nil {
			return nil, fmt.Errorf("request id: %w", err)
		}
		ids = append(ids, id)
	}
	rows, err := r.q.RatingsForRequests(ctx, sqlcgen.RatingsForRequestsParams{
		IdentityID: string(identity),
		RequestIds: ids,
	})
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		out[uuid.UUID(row.AiRequestID.Bytes).String()] = int(row.Rating)
	}
	return out, nil
}
