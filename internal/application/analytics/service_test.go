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
}

func (f fakeAnalyticsRepo) Statistics(context.Context, learner.IdentityID) (storage.Statistics, error) {
	return f.stats, f.err
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
