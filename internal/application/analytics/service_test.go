package analytics_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeAnalyticsRepo is an in-memory storage.AnalyticsRepository double:
// it returns whatever Statistics (or error) the test configures,
// exactly the shape the real postgres repo returns — raw counts with
// AcceptanceRate/CorrectionsPer1000 left zero, since deriving those is
// the service's job, not the repository's.
type fakeAnalyticsRepo struct {
	stats storage.Statistics
	err   error

	funnel         storage.VocabFunnel
	funnelErr      error
	trends         []storage.SubjectTrend
	trendsErr      error
	calibration    []storage.ConfidenceCalibration
	calibrationErr error
	agentUsage     []storage.AgentUsage
	agentUsageErr  error
	system         storage.SystemStats
	systemErr      error
}

func (f fakeAnalyticsRepo) Statistics(context.Context, learner.IdentityID) (storage.Statistics, error) {
	return f.stats, f.err
}

func (f fakeAnalyticsRepo) VocabFunnel(context.Context, learner.IdentityID) (storage.VocabFunnel, error) {
	return f.funnel, f.funnelErr
}

func (f fakeAnalyticsRepo) WeaknessTrends(context.Context, learner.IdentityID) ([]storage.SubjectTrend, error) {
	return f.trends, f.trendsErr
}

func (f fakeAnalyticsRepo) ConfidenceCalibration(context.Context, learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	return f.calibration, f.calibrationErr
}

func (f fakeAnalyticsRepo) AgentUsage(context.Context, learner.IdentityID) ([]storage.AgentUsage, error) {
	return f.agentUsage, f.agentUsageErr
}

func (f fakeAnalyticsRepo) SystemStats(context.Context, learner.IdentityID) (storage.SystemStats, error) {
	return f.system, f.systemErr
}

// TestStatisticsComputesDerivedRatios pins the exact example from the
// task brief: runes 2000, presented 6, accepted 3, rejected 1 ->
// AcceptanceRate 0.75, CorrectionsPer1000 3.0. Raw counts must pass
// through unchanged.
func TestStatisticsComputesDerivedRatios(t *testing.T) {
	repo := fakeAnalyticsRepo{stats: storage.Statistics{
		RunesWritten:         2000,
		SessionCount:         4,
		FeedbackRequests:     5,
		CorrectionsPresented: 6,
		CorrectionsAccepted:  3,
		CorrectionsRejected:  1,
		TopErrorTypes:        []storage.ErrorTypeCount{{Type: "conjugation", Count: 4}},
		// AcceptanceRate, CorrectionsPer1000 deliberately left zero: the
		// repository never sets them.
	}}
	svc := analytics.NewService(repo)

	got, err := svc.Statistics(context.Background(), "learner-a")
	if err != nil {
		t.Fatalf("Statistics returned error: %v", err)
	}
	if got.AcceptanceRate != 0.75 {
		t.Fatalf("AcceptanceRate = %v, want 0.75", got.AcceptanceRate)
	}
	if got.CorrectionsPer1000 != 3.0 {
		t.Fatalf("CorrectionsPer1000 = %v, want 3.0", got.CorrectionsPer1000)
	}
	if got.RunesWritten != 2000 || got.SessionCount != 4 || got.FeedbackRequests != 5 {
		t.Fatalf("raw counts altered: %+v", got)
	}
	if got.CorrectionsPresented != 6 || got.CorrectionsAccepted != 3 || got.CorrectionsRejected != 1 {
		t.Fatalf("raw correction counts altered: %+v", got)
	}
	if len(got.TopErrorTypes) != 1 || got.TopErrorTypes[0].Type != "conjugation" {
		t.Fatalf("TopErrorTypes not passed through: %+v", got.TopErrorTypes)
	}
}

// TestStatisticsZeroDivisionCasesReturnZero covers both denominators
// independently: no resolved corrections (accepted+rejected == 0) must
// not divide by zero for AcceptanceRate, and no runes written must not
// divide by zero for CorrectionsPer1000.
func TestStatisticsZeroDivisionCasesReturnZero(t *testing.T) {
	repo := fakeAnalyticsRepo{stats: storage.Statistics{
		RunesWritten:         0,
		CorrectionsPresented: 5,
		CorrectionsAccepted:  0,
		CorrectionsRejected:  0,
	}}
	svc := analytics.NewService(repo)

	got, err := svc.Statistics(context.Background(), "learner-a")
	if err != nil {
		t.Fatalf("Statistics returned error: %v", err)
	}
	if got.AcceptanceRate != 0 {
		t.Fatalf("AcceptanceRate = %v, want 0 (no resolved corrections)", got.AcceptanceRate)
	}
	if got.CorrectionsPer1000 != 0 {
		t.Fatalf("CorrectionsPer1000 = %v, want 0 (no runes written)", got.CorrectionsPer1000)
	}
}

// TestStatisticsPropagatesRepositoryError ensures a repository failure
// surfaces to the caller rather than being swallowed.
func TestStatisticsPropagatesRepositoryError(t *testing.T) {
	wantErr := errors.New("db down")
	repo := fakeAnalyticsRepo{err: wantErr}
	svc := analytics.NewService(repo)

	_, err := svc.Statistics(context.Background(), "learner-a")
	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestNewAnalyticsMethodsPassThroughAndPropagateErrors pins the Task 7
// additions as pure pass-throughs: the value the repository returns
// comes back unchanged, and a repository error surfaces rather than
// being swallowed — for all five, in one table so each gets the same
// two assertions without five near-identical test funcs.
func TestNewAnalyticsMethodsPassThroughAndPropagateErrors(t *testing.T) {
	wantErr := errors.New("db down")

	t.Run("VocabFunnel", func(t *testing.T) {
		want := storage.VocabFunnel{LookedUp: 10, Produced: 4, ProducedCorrectly: 3}
		svc := analytics.NewService(fakeAnalyticsRepo{funnel: want})
		got, err := svc.VocabFunnel(context.Background(), "learner-a")
		if err != nil || got != want {
			t.Fatalf("VocabFunnel() = %+v, %v, want %+v, nil", got, err, want)
		}
		if _, err := analytics.NewService(fakeAnalyticsRepo{funnelErr: wantErr}).VocabFunnel(context.Background(), "learner-a"); !errors.Is(err, wantErr) {
			t.Fatalf("VocabFunnel() err = %v, want %v", err, wantErr)
		}
	})

	t.Run("WeaknessTrends", func(t *testing.T) {
		want := []storage.SubjectTrend{{Subject: "i-adjective-past", Weeks: []storage.WeeklyCount{{Count: 2}}}}
		svc := analytics.NewService(fakeAnalyticsRepo{trends: want})
		got, err := svc.WeaknessTrends(context.Background(), "learner-a")
		if err != nil || len(got) != 1 || got[0].Subject != "i-adjective-past" {
			t.Fatalf("WeaknessTrends() = %+v, %v, want %+v, nil", got, err, want)
		}
		if _, err := analytics.NewService(fakeAnalyticsRepo{trendsErr: wantErr}).WeaknessTrends(context.Background(), "learner-a"); !errors.Is(err, wantErr) {
			t.Fatalf("WeaknessTrends() err = %v, want %v", err, wantErr)
		}
	})

	t.Run("ConfidenceCalibration", func(t *testing.T) {
		want := []storage.ConfidenceCalibration{{Confidence: 4, Attempts: 5, CorrectRate: 0.8}}
		svc := analytics.NewService(fakeAnalyticsRepo{calibration: want})
		got, err := svc.ConfidenceCalibration(context.Background(), "learner-a")
		if err != nil || len(got) != 1 || got[0] != want[0] {
			t.Fatalf("ConfidenceCalibration() = %+v, %v, want %+v, nil", got, err, want)
		}
		if _, err := analytics.NewService(fakeAnalyticsRepo{calibrationErr: wantErr}).ConfidenceCalibration(context.Background(), "learner-a"); !errors.Is(err, wantErr) {
			t.Fatalf("ConfidenceCalibration() err = %v, want %v", err, wantErr)
		}
	})

	t.Run("AgentUsage", func(t *testing.T) {
		want := []storage.AgentUsage{{Agent: "teacher", Requests: 3, SuccessRate: 1, AvgLatencyMS: 200}}
		svc := analytics.NewService(fakeAnalyticsRepo{agentUsage: want})
		got, err := svc.AgentUsage(context.Background(), "learner-a")
		if err != nil || len(got) != 1 || got[0] != want[0] {
			t.Fatalf("AgentUsage() = %+v, %v, want %+v, nil", got, err, want)
		}
		if _, err := analytics.NewService(fakeAnalyticsRepo{agentUsageErr: wantErr}).AgentUsage(context.Background(), "learner-a"); !errors.Is(err, wantErr) {
			t.Fatalf("AgentUsage() err = %v, want %v", err, wantErr)
		}
	})

	t.Run("SystemStats", func(t *testing.T) {
		want := storage.SystemStats{LearningEvents: 7, EventsByType: map[string]int{"quiz.answered": 7}, AIRequests: 2}
		svc := analytics.NewService(fakeAnalyticsRepo{system: want})
		got, err := svc.SystemStats(context.Background(), "learner-a")
		if err != nil || got.LearningEvents != 7 || got.AIRequests != 2 {
			t.Fatalf("SystemStats() = %+v, %v, want %+v, nil", got, err, want)
		}
		if _, err := analytics.NewService(fakeAnalyticsRepo{systemErr: wantErr}).SystemStats(context.Background(), "learner-a"); !errors.Is(err, wantErr) {
			t.Fatalf("SystemStats() err = %v, want %v", err, wantErr)
		}
	})
}
