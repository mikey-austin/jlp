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

// VocabFunnel, WeaknessTrends, ConfidenceCalibration, AgentUsage, and
// SystemStats are plain pass-throughs to the repository, unlike
// Statistics above: each repository query already returns a value
// that's either raw (no ratio involved) or a ratio computed from a
// GROUP BY bucket guaranteed non-empty (so it can never be the
// division-by-a-legitimate-zero case Statistics' two ratios have to
// guard against) — see storage.AnalyticsRepository's doc comments on
// each method for why.

func (s *Service) VocabFunnel(ctx context.Context, identity learner.IdentityID) (storage.VocabFunnel, error) {
	return s.repo.VocabFunnel(ctx, identity)
}

func (s *Service) WeaknessTrends(ctx context.Context, identity learner.IdentityID) ([]storage.SubjectTrend, error) {
	return s.repo.WeaknessTrends(ctx, identity)
}

func (s *Service) ConfidenceCalibration(ctx context.Context, identity learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	return s.repo.ConfidenceCalibration(ctx, identity)
}

func (s *Service) AgentUsage(ctx context.Context, identity learner.IdentityID) ([]storage.AgentUsage, error) {
	return s.repo.AgentUsage(ctx, identity)
}

func (s *Service) SystemStats(ctx context.Context, identity learner.IdentityID) (storage.SystemStats, error) {
	return s.repo.SystemStats(ctx, identity)
}
