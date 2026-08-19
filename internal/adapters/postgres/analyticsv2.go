package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// weeklyTrendWeeks is how many trailing ISO weeks WeaknessTrends charts
// per subject — a fixed 8-week window per the task brief.
const weeklyTrendWeeks = 8

// VocabFunnel assembles storage.VocabFunnel from db/queries/analytics_v2.sql's
// VocabFunnel query — a single identity-scoped sum over vocabulary_items.
func (r *AnalyticsRepository) VocabFunnel(ctx context.Context, identity learner.IdentityID) (storage.VocabFunnel, error) {
	row, err := r.q.VocabFunnel(ctx, string(identity))
	if err != nil {
		return storage.VocabFunnel{}, err
	}
	return storage.VocabFunnel{
		LookedUp:          int(row.LookedUp),
		Produced:          int(row.Produced),
		ProducedCorrectly: int(row.ProducedCorrectly),
	}, nil
}

// WeaknessTrends returns an 8-week, zero-filled occurrence sparkline
// for each of identity's top 3 live weaknesses (TopWeaknessSubjects).
// Each subject's weeks always has exactly weeklyTrendWeeks entries,
// oldest first, one per isoWeekStarts(now, weeklyTrendWeeks) — a week
// with no matching learning_events rows is an explicit
// storage.WeeklyCount{Count: 0}, not a missing entry, so a caller
// zipping Weeks against a fixed set of column headers/x-axis positions
// never has to special-case a gap.
func (r *AnalyticsRepository) WeaknessTrends(ctx context.Context, identity learner.IdentityID) ([]storage.SubjectTrend, error) {
	subjects, err := r.q.TopWeaknessSubjects(ctx, string(identity))
	if err != nil {
		return nil, err
	}

	weekStarts := isoWeekStarts(time.Now(), weeklyTrendWeeks)
	since := weekStarts[0]

	out := make([]storage.SubjectTrend, 0, len(subjects))
	for _, s := range subjects {
		rows, err := r.q.SubjectOccurrencesByWeek(ctx, sqlcgen.SubjectOccurrencesByWeekParams{
			IdentityID: string(identity),
			OccurredAt: pgtype.Timestamptz{Time: since, Valid: true},
			Column3:    s.SubjectType,
			Subject:    s.Subject,
		})
		if err != nil {
			return nil, err
		}

		byWeek := make(map[string]int, len(rows))
		for _, row := range rows {
			if !row.WeekStart.Valid {
				continue
			}
			byWeek[row.WeekStart.Time.UTC().Format(weekKeyLayout)] = int(row.Count)
		}

		weeks := make([]storage.WeeklyCount, len(weekStarts))
		for i, ws := range weekStarts {
			weeks[i] = storage.WeeklyCount{WeekStart: ws, Count: byWeek[ws.Format(weekKeyLayout)]}
		}
		out = append(out, storage.SubjectTrend{Subject: s.Subject, Weeks: weeks})
	}
	return out, nil
}

// ConfidenceCalibration assembles storage.ConfidenceCalibration rows
// from the ConfidenceCalibration query, leaving CorrectRate at its
// zero value — application/analytics.Service computes it from
// Corrects/Attempts, the same raw-data-in-SQL/ratio-in-Go split
// Statistics uses (see storage.ConfidenceCalibration's doc comment).
func (r *AnalyticsRepository) ConfidenceCalibration(ctx context.Context, identity learner.IdentityID) ([]storage.ConfidenceCalibration, error) {
	rows, err := r.q.ConfidenceCalibration(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.ConfidenceCalibration, 0, len(rows))
	for _, row := range rows {
		// row.Confidence is a nullable pgtype.Int4 only because the
		// exercise_attempts.confidence column itself is nullable — the
		// query's "confidence IS NOT NULL" filter guarantees every
		// returned row actually has a value.
		out = append(out, storage.ConfidenceCalibration{
			Confidence: int(row.Confidence.Int32),
			Attempts:   int(row.Attempts),
			Corrects:   int(row.Corrects),
		})
	}
	return out, nil
}

// AgentUsage assembles storage.AgentUsage rows from AgentUsageStats,
// leaving SuccessRate at its zero value — application/analytics.Service
// computes it from Successes/Requests (see AgentUsage's doc comment).
func (r *AnalyticsRepository) AgentUsage(ctx context.Context, identity learner.IdentityID) ([]storage.AgentUsage, error) {
	rows, err := r.q.AgentUsageStats(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	out := make([]storage.AgentUsage, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.AgentUsage{
			Agent:        row.Agent,
			Requests:     int(row.Requests),
			Successes:    int(row.Successes),
			AvgLatencyMS: int(row.AvgLatencyMs),
		})
	}
	return out, nil
}

// SystemStats assembles storage.SystemStats from two identity-scoped
// queries: LearningEventsByType (which LearningEvents is summed from,
// so the total and the breakdown can never disagree) and
// CountAIRequestsForIdentity.
func (r *AnalyticsRepository) SystemStats(ctx context.Context, identity learner.IdentityID) (storage.SystemStats, error) {
	byType, err := r.q.LearningEventsByType(ctx, string(identity))
	if err != nil {
		return storage.SystemStats{}, err
	}
	aiRequests, err := r.q.CountAIRequestsForIdentity(ctx, string(identity))
	if err != nil {
		return storage.SystemStats{}, err
	}

	stats := storage.SystemStats{
		EventsByType: make(map[string]int, len(byType)),
		AIRequests:   int(aiRequests),
	}
	for _, row := range byType {
		stats.EventsByType[row.Type] = int(row.Count)
		stats.LearningEvents += int(row.Count)
	}
	return stats, nil
}

// weekKeyLayout is the map-key format WeaknessTrends uses to match
// Postgres-returned week-start dates against Go-computed ones —
// comparing time.Time values directly as map keys is fragile across a
// pgtype.Date/time.Time round-trip (Location pointer identity isn't
// guaranteed to match even for the same instant), so both sides are
// canonicalized through this string format instead.
const weekKeyLayout = "2006-01-02"

// isoWeekStarts returns n consecutive ISO week start dates (Mondays,
// UTC midnight), oldest first, ending with the week containing now.
// Matches SubjectOccurrencesByWeek's `date_trunc('week', occurred_at AT
// TIME ZONE 'UTC')` bucketing exactly, so a week with zero matching
// rows and a week that was never queried are indistinguishable to the
// caller — WeaknessTrends zero-fills every entry in this slice.
func isoWeekStarts(now time.Time, n int) []time.Time {
	now = now.UTC()
	weekday := int(now.Weekday()) // Sunday=0 .. Saturday=6
	if weekday == 0 {
		weekday = 7 // ISO: Monday=1 .. Sunday=7
	}
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	currentWeekStart := today.AddDate(0, 0, -(weekday - 1))

	starts := make([]time.Time, n)
	for i := 0; i < n; i++ {
		starts[i] = currentWeekStart.AddDate(0, 0, -7*(n-1-i))
	}
	return starts
}

// PracticeStats implements storage.AnalyticsRepository.PracticeStats —
// see that type's doc comment for why this reads learning_events rather
// than exercise_attempts.
func (r *AnalyticsRepository) PracticeStats(ctx context.Context, identity learner.IdentityID) (storage.PracticeStats, error) {
	row, err := r.q.PracticeStats(ctx, string(identity))
	if err != nil {
		return storage.PracticeStats{}, err
	}
	return storage.PracticeStats{
		Answered: int(row.Answered),
		Correct:  int(row.Correct),
		Words:    int(row.Words),
		Concepts: int(row.Concepts),
	}, nil
}
