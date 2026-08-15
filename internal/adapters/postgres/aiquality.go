package postgres

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// AIQualityRepository aggregates ai_requests (LEFT JOIN ai_ratings) for
// the /ai page's プロバイダー比較 and プロンプト品質 sections (PRD §26).
// Both queries (see db/queries/ai_quality.sql) scope by identity_id on
// every table involved, so results never include another identity's
// requests or ratings.
type AIQualityRepository struct{ q *sqlcgen.Queries }

func NewAIQualityRepository(pool *pgxpool.Pool) *AIQualityRepository {
	return &AIQualityRepository{q: sqlcgen.New(pool)}
}

func (r *AIQualityRepository) ByProvider(ctx context.Context, identity learner.IdentityID) ([]storage.ProviderStats, error) {
	rows, err := r.q.StatsByProvider(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.ProviderStats, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.ProviderStats{
			Provider:     row.Provider,
			Model:        row.Model,
			Requests:     int(row.Requests),
			SuccessRate:  row.SuccessRate,
			AvgRating:    row.AvgRating,
			AvgLatencyMS: int(row.AvgLatencyMs),
			P95LatencyMS: int(row.P95LatencyMs),
			TotalCostUSD: row.TotalCostUsd,
		})
	}
	return out, nil
}

func (r *AIQualityRepository) ByPrompt(ctx context.Context, identity learner.IdentityID) ([]storage.PromptStats, error) {
	rows, err := r.q.StatsByPrompt(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.PromptStats, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.PromptStats{
			PromptName:    row.PromptName,
			PromptVersion: row.PromptVersion,
			Requests:      int(row.Requests),
			AvgRating:     row.AvgRating,
			SuccessRate:   row.SuccessRate,
		})
	}
	return out, nil
}
