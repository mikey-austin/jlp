package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type AnalyticsRepository struct{ q *sqlcgen.Queries }

func NewAnalyticsRepository(pool *pgxpool.Pool) *AnalyticsRepository {
	return &AnalyticsRepository{q: sqlcgen.New(pool)}
}

// Statistics assembles storage.Statistics from five identity-scoped
// queries (see db/queries/analytics.sql), leaving AcceptanceRate and
// CorrectionsPer1000 at their zero value — application/analytics.Service
// computes those from the raw counts here.
//
// CorrectionsPresented sums CountCorrectionsByStatus across every
// status: a correction row starts "presented" and only its status
// changes as the learner accepts/rejects it (see
// storage.CorrectionRecord), so "presented" means every correction ever
// shown, not just ones still awaiting a decision.
func (r *AnalyticsRepository) Statistics(ctx context.Context, identity learner.IdentityID) (storage.Statistics, error) {
	id := string(identity)

	runes, err := r.q.CountRunesWritten(ctx, id)
	if err != nil {
		return storage.Statistics{}, err
	}
	sessionCount, err := r.q.CountSessions(ctx, id)
	if err != nil {
		return storage.Statistics{}, err
	}
	feedbackRequests, err := r.q.CountFeedbackRequests(ctx, id)
	if err != nil {
		return storage.Statistics{}, err
	}
	byStatus, err := r.q.CountCorrectionsByStatus(ctx, id)
	if err != nil {
		return storage.Statistics{}, err
	}
	topTypes, err := r.q.TopErrorTypes(ctx, id)
	if err != nil {
		return storage.Statistics{}, err
	}

	stats := storage.Statistics{
		RunesWritten:     int(runes),
		SessionCount:     int(sessionCount),
		FeedbackRequests: int(feedbackRequests),
	}
	for _, row := range byStatus {
		stats.CorrectionsPresented += int(row.Count)
		switch row.Status {
		case "accepted":
			stats.CorrectionsAccepted = int(row.Count)
		case "rejected":
			stats.CorrectionsRejected = int(row.Count)
		}
	}

	stats.TopErrorTypes = make([]storage.ErrorTypeCount, 0, len(topTypes))
	for _, row := range topTypes {
		stats.TopErrorTypes = append(stats.TopErrorTypes, storage.ErrorTypeCount{Type: row.Type, Count: int(row.Count)})
	}
	return stats, nil
}
