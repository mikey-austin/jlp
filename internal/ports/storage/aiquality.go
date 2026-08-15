package storage

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// ProviderStats is one (provider, model) pair's aggregate quality
// numbers across every ai_requests row an identity owns — the /ai
// page's プロバイダー比較 section (PRD §26).
type ProviderStats struct {
	Provider, Model string
	Requests        int
	SuccessRate     float64 // 0..1
	AvgRating       float64 // 0 when unrated
	AvgLatencyMS    int
	P95LatencyMS    int
	TotalCostUSD    float64
}

// PromptStats is one (prompt_name, prompt_version) pair's aggregate
// quality numbers — the /ai page's プロンプト品質 section (PRD §26),
// distinguishing e.g. teacher.feedback v2 from v3 so a prompt change's
// effect on rating/success rate is visible.
type PromptStats struct {
	PromptName, PromptVersion string
	Requests                  int
	AvgRating                 float64
	SuccessRate               float64
}

// AIQualityRepository aggregates ai_requests (LEFT JOIN ai_ratings) for
// one identity, scoped the same way every other identity-scoped
// repository in this package is: results never include another
// identity's requests or ratings.
type AIQualityRepository interface {
	ByProvider(ctx context.Context, identity learner.IdentityID) ([]ProviderStats, error)
	ByPrompt(ctx context.Context, identity learner.IdentityID) ([]PromptStats, error)
}
