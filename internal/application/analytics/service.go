// Package analytics is the application-layer statistics service: it
// wraps storage.AnalyticsRepository's raw counts and computes the two
// ratios (AcceptanceRate, CorrectionsPer1000) that depend on division
// by a value that can legitimately be zero. Keeping that arithmetic
// here, out of SQL, makes it directly unit-testable — see
// service_test.go for the pinned example from the task brief.
package analytics

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Service struct {
	repo storage.AnalyticsRepository
}

func NewService(repo storage.AnalyticsRepository) *Service {
	return &Service{repo: repo}
}

// Statistics returns identity's aggregate storage.Statistics, filling
// in AcceptanceRate and CorrectionsPer1000 from the raw counts the
// repository returns (which leaves both zeroed). Both ratios default to
// 0 when their denominator is 0, rather than dividing by it: a learner
// with no resolved corrections yet, or no writing yet, has an
// undefined rate, and 0 is the sensible display value for that on the
// dashboard.
func (s *Service) Statistics(ctx context.Context, identity learner.IdentityID) (storage.Statistics, error) {
	stats, err := s.repo.Statistics(ctx, identity)
	if err != nil {
		return storage.Statistics{}, err
	}

	if resolved := stats.CorrectionsAccepted + stats.CorrectionsRejected; resolved > 0 {
		stats.AcceptanceRate = float64(stats.CorrectionsAccepted) / float64(resolved)
	}
	if stats.RunesWritten > 0 {
		stats.CorrectionsPer1000 = float64(stats.CorrectionsPresented) / float64(stats.RunesWritten) * 1000
	}
	return stats, nil
}
