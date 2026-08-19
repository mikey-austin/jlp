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

// VocabFunnel, WeaknessTrends, and SystemStats are plain pass-throughs
// to the repository: none of their fields is a derived ratio.
// ConfidenceCalibration and AgentUsage below follow Statistics' split
// instead — the repository returns raw counts, this service derives
// the rate — even though, unlike Statistics' two ratios, their
// denominator (Attempts/Requests) can never actually be zero (each row
// comes from a non-empty GROUP BY bucket): the convention is kept
// uniform across every ratio this package computes, which is what
// buys the fast, DB-free zero-denominator unit coverage below (see
// TestConfidenceCalibrationZeroAttemptsReturnsZeroRate/
// TestAgentUsageZeroRequestsReturnsZeroRate) rather than relying on an
// integration test to exercise that path.

func (s *Service) VocabFunnel(ctx context.Context, identity learner.IdentityID) (storage.VocabFunnel, error) {
	return s.repo.VocabFunnel(ctx, identity)
}

func (s *Service) WeaknessTrends(ctx context.Context, identity learner.IdentityID) ([]storage.SubjectTrend, error) {
	return s.repo.WeaknessTrends(ctx, identity)
}

// ConfidenceCalibration returns identity's calibration rows, filling in
// CorrectRate from each row's raw Corrects/Attempts (which the
// repository leaves zeroed) — 0 when Attempts is 0 rather than
// dividing by it.
func (s *Service) ConfidenceCalibration(ctx context.Context, identity learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	rows, err := s.repo.ConfidenceCalibration(ctx, identity)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Attempts > 0 {
			rows[i].CorrectRate = float64(rows[i].Corrects) / float64(rows[i].Attempts)
		}
	}
	return rows, nil
}

// AgentUsage returns identity's per-agent usage rows, filling in
// SuccessRate from each row's raw Successes/Requests (which the
// repository leaves zeroed) — 0 when Requests is 0 rather than
// dividing by it.
func (s *Service) AgentUsage(ctx context.Context, identity learner.IdentityID) ([]storage.AgentUsage, error) {
	rows, err := s.repo.AgentUsage(ctx, identity)
	if err != nil {
		return nil, err
	}
	for i := range rows {
		if rows[i].Requests > 0 {
			rows[i].SuccessRate = float64(rows[i].Successes) / float64(rows[i].Requests)
		}
	}
	return rows, nil
}

func (s *Service) SystemStats(ctx context.Context, identity learner.IdentityID) (storage.SystemStats, error) {
	return s.repo.SystemStats(ctx, identity)
}

// PracticeStats is /learner's 練習 section — see
// storage.PracticeStats.
func (s *Service) PracticeStats(ctx context.Context, identity learner.IdentityID) (storage.PracticeStats, error) {
	return s.repo.PracticeStats(ctx, identity)
}
