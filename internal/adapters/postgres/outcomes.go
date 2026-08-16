package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mikeyaustin/jlp/internal/adapters/postgres/sqlcgen"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// OutcomeRepository reads the raw evidence behind /outcomes (Phase 4
// Task 9, PRD §52/§72/§73) from db/queries/outcomes.sql.
//
// It returns counts and timestamps and nothing else: ConceptOutcome.
// Retired, CalibrationTrend's two ratios, and WeeklyRate.Per1000 are
// all left at their zero values for application/outcomes.Analyser to
// derive — the same raw-counts-here / judgements-there split
// AnalyticsRepository already uses for AcceptanceRate and CorrectRate,
// and the reason none of this file contains a threshold.
type OutcomeRepository struct{ q *sqlcgen.Queries }

func NewOutcomeRepository(pool *pgxpool.Pool) *OutcomeRepository {
	return &OutcomeRepository{q: sqlcgen.New(pool)}
}

// ConceptOutcomes assembles one storage.ConceptOutcome per concept this
// identity has been corrected on, ordered by slug.
//
// It runs three queries rather than one: the per-concept correction
// bounds/windows, the independent-solve counts (which come from
// learning_events, not corrections), and the produced-correctly counts
// (which come from learning_events joined through the vocabulary bank).
// Folding all three into a single statement would need two more
// FULL OUTER JOINs over unrelated aggregate sets purely to save two
// round trips on a page that loads once — the three-map assembly below
// is the cheaper thing to read and the cheaper thing to get right.
//
// Concepts appear here ONLY if they have at least one resolved
// correction; the two supplementary maps can therefore only ever add
// counts to a concept that already has a row, never introduce one.
// (An independent solve or a correct production recorded against a
// concept with no corrections at all would be nonsensical — both are
// downstream of a correction by construction.)
func (r *OutcomeRepository) ConceptOutcomes(ctx context.Context, identity learner.IdentityID) ([]storage.ConceptOutcome, error) {
	rows, err := r.q.ConceptCorrectionOutcomes(ctx, sqlcgen.ConceptCorrectionOutcomesParams{
		IdentityID: string(identity),
		// The two measurement windows come from the port's own
		// constants, never from a literal here — see
		// storage.OutcomeBaselineWindow's doc comment for why they live
		// there.
		//
		// This reads the wall clock directly, unlike
		// application/outcomes.Analyser, which takes an injected one.
		// The split is deliberate: the analyser's clock exists so its
		// JUDGEMENT boundaries can be walked exactly in a unit test,
		// whereas this window only bounds a query whose own tests seed
		// rows relative to the same wall clock (see
		// outcomes_integration_test.go). Threading a clock through
		// every repository method to serve a test that doesn't need one
		// would buy nothing — no other repository in this package does
		// it either.
		Column2: int64(storage.OutcomeBaselineWindow / time.Second),
		Column3: pgtype.Timestamptz{Time: time.Now().UTC().Add(-storage.OutcomeRecentWindow), Valid: true},
	})
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		// Nothing has been corrected yet. Skipping the two
		// supplementary queries here isn't just an optimisation: it is
		// the brand-new-learner path, and it must cost one query, not
		// three.
		return nil, nil
	}

	solveRows, err := r.q.ConceptIndependentSolves(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	solves := make(map[string]int, len(solveRows))
	for _, row := range solveRows {
		solves[row.Slug] = int(row.Solves)
	}

	producedRows, err := r.q.ConceptProducedCorrectly(ctx, string(identity))
	if err != nil {
		return nil, err
	}
	produced := make(map[string]int, len(producedRows))
	for _, row := range producedRows {
		produced[row.Slug] = int(row.ProducedCorrectly)
	}

	out := make([]storage.ConceptOutcome, 0, len(rows))
	for _, row := range rows {
		out = append(out, storage.ConceptOutcome{
			Slug:              row.Slug,
			Name:              row.Name,
			FirstCorrectedAt:  row.FirstCorrectedAt.Time.UTC(),
			LastCorrectedAt:   row.LastCorrectedAt.Time.UTC(),
			TotalCorrections:  int(row.TotalCorrections),
			CorrectionsBefore: int(row.CorrectionsBefore),
			CorrectionsAfter:  int(row.CorrectionsAfter),
			IndependentSolves: solves[row.Slug],
			ProducedCorrectly: produced[row.Slug],
			// Retired is deliberately not set — see the type's doc
			// comment.
		})
	}
	return out, nil
}

// CalibrationTrend returns exactly weeks zero-filled ISO weeks, oldest
// first, ending with the week containing now — the same
// isoWeekStarts/weekKeyLayout zero-fill AnalyticsRepository.
// WeaknessTrends uses, so /outcomes and the home dashboard bucket weeks
// identically. A week with no confidence-rated attempts is an explicit
// Attempts == 0 entry, never a gap; the ratios stay zero for the caller
// to derive (and for the page to render as "no data" rather than 0%).
func (r *OutcomeRepository) CalibrationTrend(ctx context.Context, identity learner.IdentityID, weeks int) ([]storage.CalibrationTrend, error) {
	if weeks <= 0 {
		return nil, nil
	}
	weekStarts := isoWeekStarts(time.Now(), weeks)

	rows, err := r.q.WeeklyCalibration(ctx, sqlcgen.WeeklyCalibrationParams{
		IdentityID: string(identity),
		CreatedAt:  pgtype.Timestamptz{Time: weekStarts[0], Valid: true},
	})
	if err != nil {
		return nil, err
	}

	byWeek := make(map[string]sqlcgen.WeeklyCalibrationRow, len(rows))
	for _, row := range rows {
		if !row.WeekStart.Valid {
			continue
		}
		byWeek[row.WeekStart.Time.UTC().Format(weekKeyLayout)] = row
	}

	out := make([]storage.CalibrationTrend, len(weekStarts))
	for i, ws := range weekStarts {
		row := byWeek[ws.Format(weekKeyLayout)]
		out[i] = storage.CalibrationTrend{
			WeekStart:       ws,
			Attempts:        int(row.Attempts),
			ConfidenceTotal: int(row.ConfidenceTotal),
			Corrects:        int(row.Corrects),
		}
	}
	return out, nil
}

// AssistanceFading returns exactly weeks zero-filled ISO weeks, oldest
// first, ending with the week containing now — same bucketing and
// zero-fill contract as CalibrationTrend above. Runes == 0 marks a week
// with nothing submitted for review, which is a week with nothing to
// measure, NOT a week that needed no help; Per1000 stays zero for the
// caller to derive.
func (r *OutcomeRepository) AssistanceFading(ctx context.Context, identity learner.IdentityID, weeks int) ([]storage.WeeklyRate, error) {
	if weeks <= 0 {
		return nil, nil
	}
	weekStarts := isoWeekStarts(time.Now(), weeks)

	rows, err := r.q.WeeklyAssistanceRate(ctx, sqlcgen.WeeklyAssistanceRateParams{
		IdentityID: string(identity),
		CreatedAt:  pgtype.Timestamptz{Time: weekStarts[0], Valid: true},
	})
	if err != nil {
		return nil, err
	}

	byWeek := make(map[string]sqlcgen.WeeklyAssistanceRateRow, len(rows))
	for _, row := range rows {
		if !row.WeekStart.Valid {
			continue
		}
		byWeek[row.WeekStart.Time.UTC().Format(weekKeyLayout)] = row
	}

	out := make([]storage.WeeklyRate, len(weekStarts))
	for i, ws := range weekStarts {
		row := byWeek[ws.Format(weekKeyLayout)]
		out[i] = storage.WeeklyRate{
			WeekStart:   ws,
			Runes:       int(row.Runes),
			Corrections: int(row.Corrections),
		}
	}
	return out, nil
}
