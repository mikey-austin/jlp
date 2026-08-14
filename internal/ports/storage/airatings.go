package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// AIRating is one learner's star rating (1..5) of a single AI request's
// response — the AI requests page's rating column and the feedback
// partial's rating widget (Task 15) both key off AIRequestID.
type AIRating struct {
	ID, AIRequestID string
	IdentityID      learner.IdentityID
	Rating          int
	CreatedAt       time.Time
}

// AIRatingRepository persists learner ratings of AI responses. There is
// at most one rating per (ai_request_id, identity): rating the same
// request again replaces the previous value rather than accumulating
// duplicate rows.
type AIRatingRepository interface {
	// Upsert inserts r, or — if (r.AIRequestID, r.IdentityID) already has
	// a rating — replaces its value.
	Upsert(ctx context.Context, r AIRating) error
	// ForRequests returns identity's ratings for requestIDs, keyed by
	// AIRequestID. A requestID the identity hasn't rated (or that
	// belongs to another identity) is simply absent from the map.
	ForRequests(ctx context.Context, identity learner.IdentityID, requestIDs []string) (map[string]int, error)
}
