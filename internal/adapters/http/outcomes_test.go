package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/outcomes"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// outcomesNow is the fixed clock every /outcomes page test judges
// against, so a seeded concept's "120 days ago" means the same thing
// whenever the suite runs.
var outcomesNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

const outcomesDay = 24 * time.Hour

// fakeOutcomeRepo is an in-memory storage.OutcomeRepository double for
// the /outcomes page tests. The page goes through the REAL
// application/outcomes.Analyser (not a stubbed report), so these tests
// also pin that the handler renders what the analyser actually
// classifies rather than a shape only the test believes in.
type fakeOutcomeRepo struct {
	concepts    []storage.ConceptOutcome
	conceptsErr error
	calibration []storage.CalibrationTrend
	fading      []storage.WeeklyRate
}

func (f *fakeOutcomeRepo) ConceptOutcomes(context.Context, learner.IdentityID) ([]storage.ConceptOutcome, error) {
	return f.concepts, f.conceptsErr
}

func (f *fakeOutcomeRepo) CalibrationTrend(_ context.Context, _ learner.IdentityID, weeks int) ([]storage.CalibrationTrend, error) {
	if f.calibration != nil {
		return f.calibration, nil
	}
	out := make([]storage.CalibrationTrend, weeks)
	for i := range out {
		out[i] = storage.CalibrationTrend{WeekStart: outcomesNow.Add(-time.Duration(weeks-1-i) * 7 * outcomesDay)}
	}
	return out, nil
}

func (f *fakeOutcomeRepo) AssistanceFading(_ context.Context, _ learner.IdentityID, weeks int) ([]storage.WeeklyRate, error) {
	if f.fading != nil {
		return f.fading, nil
	}
	out := make([]storage.WeeklyRate, weeks)
	for i := range out {
		out[i] = storage.WeeklyRate{WeekStart: outcomesNow.Add(-time.Duration(weeks-1-i) * 7 * outcomesDay)}
	}
	return out, nil
}

func outcomesTestOptions(repo *fakeOutcomeRepo) Options {
	opts := testOptions()
	opts.Outcomes = outcomes.NewAnalyser(repo, func() time.Time { return outcomesNow })
	return opts
}

func getOutcomes(t *testing.T, opts Options) string {
	t.Helper()
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /outcomes status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

func outcomeRow(slug string, firstAgo, lastAgo time.Duration, total, before, after int) storage.ConceptOutcome {
	return storage.ConceptOutcome{
		Slug:              slug,
		Name:              slug + "-name",
		FirstCorrectedAt:  outcomesNow.Add(-firstAgo),
		LastCorrectedAt:   outcomesNow.Add(-lastAgo),
		TotalCorrections:  total,
		CorrectionsBefore: before,
		CorrectionsAfter:  after,
	}
}

// TestOutcomesPageWithNoDataIsHonest is the case every new learner
// sees, so it is the first one pinned: the page must say there is not
// enough data, and must NOT draw a trend, quote a rate, or show a 0%.
func TestOutcomesPageWithNoDataIsHonest(t *testing.T) {
	body := getOutcomes(t, outcomesTestOptions(&fakeOutcomeRepo{}))

	if !strings.Contains(body, "Not enough data yet to say whether corrections are improving later production.") {
		t.Errorf("missing the honest zero-data headline: %s", body)
	}
	// Both trends replaced by an explicit explanation, not an empty
	// chart: an all-zero polyline would read as a flat (or falling) line.
	if strings.Contains(body, `<polyline`) {
		t.Errorf("zero-data page drew a sparkline: %s", body)
	}
	for _, want := range []string{
		"添削に出した文章が2週分に届いていないため、推移はまだ描けません。",
		"自信を記録した練習がまだないため、較正は測れません。",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing explicit empty state %q", want)
		}
	}
	if strings.Contains(body, "0%") {
		t.Errorf("zero-data page shows a 0%% figure: %s", body)
	}
}

// TestOutcomesPageRendersEveryGroupAndTheExcludedCount covers Step 3's
// "sections render" plus the requirement that the page states how many
// concepts were left unjudged and why.
func TestOutcomesPageRendersEveryGroupAndTheExcludedCount(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		func() storage.ConceptOutcome {
			c := outcomeRow("improving-slug", 120*outcomesDay, outcomesDay, 9, 5, 1)
			c.IndependentSolves = 4
			c.ProducedCorrectly = 2
			return c
		}(),
		outcomeRow("persistent-slug", 120*outcomesDay, outcomesDay, 9, 5, 6),
		outcomeRow("retired-slug", 200*outcomesDay, 90*outcomesDay, 4, 4, 0),
		outcomeRow("thin-slug", 200*outcomesDay, outcomesDay, 2, 1, 1),
		outcomeRow("recent-slug", 3*outcomesDay, outcomesDay, 9, 5, 5),
	}}
	body := getOutcomes(t, outcomesTestOptions(repo))

	for _, want := range []string{
		// Section headings.
		"改善した概念", "まだ続いている概念", "収束した概念", "判定しなかった概念",
		"支援の逓減", "自信の較正",
		// Every concept appears in exactly the group it belongs to, and
		// links back to its own catalog page.
		`href="/grammar/improving-slug"`, "improving-slug-name",
		`href="/grammar/persistent-slug"`, `href="/grammar/retired-slug"`,
		`href="/grammar/thin-slug"`, `href="/grammar/recent-slug"`,
		// The excluded count, stated as a number rather than implied by
		// the table's length.
		"判定しなかった概念（2件）",
		"判定できた概念は3件、データ不足で判定しなかった概念は2件です。",
		// The per-concept reasons, not a generic "insufficient data".
		"only 1 correction in the first 30 days",
		"only 3 days of history",
		// Supporting evidence columns.
		"自力正答", "語彙の正用",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /outcomes body missing %q", want)
		}
	}

	// The headline states every group AND the exclusions.
	if !strings.Contains(body, "1 concept retired, 1 improving, 1 still recurring, 2 excluded for insufficient data.") {
		t.Errorf("headline missing or wrong: %s", body)
	}
}

// TestOutcomesPageDrawsTheFadingTrendOnlyWithTwoMeasurableWeeks pins
// the chart's own honesty rule, on both sides of the boundary.
func TestOutcomesPageDrawsTheFadingTrendOnlyWithTwoMeasurableWeeks(t *testing.T) {
	oneWeek := &fakeOutcomeRepo{fading: []storage.WeeklyRate{
		{WeekStart: outcomesNow.Add(-7 * outcomesDay)},
		{WeekStart: outcomesNow, Runes: 10000, Corrections: 34},
	}}
	body := getOutcomes(t, outcomesTestOptions(oneWeek))
	if strings.Contains(body, `<polyline`) {
		t.Errorf("drew a trend from a single measurable week: %s", body)
	}
	if !strings.Contains(body, "添削に出した文章が2週分に届いていないため、推移はまだ描けません。") {
		t.Errorf("no explanation replaced the missing chart: %s", body)
	}
	// The week's own numbers are still shown: they imply no trend, so
	// withholding them would be its own small dishonesty.
	if !strings.Contains(body, `<td data-label="修正/1000字">3.4</td>`) {
		t.Errorf("the one measurable week's numbers were hidden with the chart: %s", body)
	}

	twoWeeks := &fakeOutcomeRepo{fading: []storage.WeeklyRate{
		{WeekStart: outcomesNow.Add(-7 * outcomesDay), Runes: 10000, Corrections: 81},
		{WeekStart: outcomesNow, Runes: 10000, Corrections: 34},
	}}
	body = getOutcomes(t, outcomesTestOptions(twoWeeks))
	if !strings.Contains(body, `<polyline`) {
		t.Errorf("did not draw a trend from two measurable weeks: %s", body)
	}
	for _, want := range []string{
		"Corrections per 1,000 characters fell from 8.1 to 3.4 over 2 weeks.",
		"8.1", "3.4", "修正/1000字",
		"直近2週のうち計測できた週：2",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fading section missing %q: %s", want, body)
		}
	}
}

// TestOutcomesPageShowsUnmeasuredWeeksAsDashesNotZeroes: a week with
// nothing submitted for review has no rate — rendering it as 0.0 would
// claim a week of flawless writing that never happened.
func TestOutcomesPageShowsUnmeasuredWeeksAsDashesNotZeroes(t *testing.T) {
	repo := &fakeOutcomeRepo{
		fading: []storage.WeeklyRate{
			{WeekStart: outcomesNow.Add(-14 * outcomesDay), Runes: 10000, Corrections: 81},
			{WeekStart: outcomesNow.Add(-7 * outcomesDay)},
			{WeekStart: outcomesNow, Runes: 10000, Corrections: 34},
		},
		calibration: []storage.CalibrationTrend{
			{WeekStart: outcomesNow.Add(-7 * outcomesDay)},
			{WeekStart: outcomesNow, Attempts: 4, ConfidenceTotal: 16, Corrects: 2},
		},
	}
	body := getOutcomes(t, outcomesTestOptions(repo))

	// The unmeasured middle week must reach the table as a dash. This
	// pins the actual cell rather than searching the whole body for
	// "0.0", which would false-positive on the sparkline's own polyline
	// coordinates.
	if strings.Contains(body, `<td data-label="修正/1000字">0.0</td>`) {
		t.Errorf("an unmeasured week was rendered as a 0.0 rate: %s", body)
	}
	if !strings.Contains(body, `<td data-label="修正/1000字">—</td>`) {
		t.Errorf("the unmeasured week is not rendered as a dash: %s", body)
	}
	// 16/4 = 4.0 mean confidence, 2/4 = 50% correct: 0.8 - 0.5 = +30pt.
	for _, want := range []string{"4.0 / 5", "50%", "30pt"} {
		if !strings.Contains(body, want) {
			t.Errorf("calibration row missing %q: %s", want, body)
		}
	}
}

// TestOutcomesPageHasNoGamification is a standing PRD §56 guard on the
// rendered page, not just on the headline string.
func TestOutcomesPageHasNoGamification(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		outcomeRow("improving-slug", 120*outcomesDay, outcomesDay, 9, 5, 1),
	}}
	body := getOutcomes(t, outcomesTestOptions(repo))

	for _, forbidden := range []string{"連続", "ストリーク", "streak", "バッジ", "レベルアップ", "おめでとう", "🎉", "🔥"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/outcomes contains gamification token %q", forbidden)
		}
	}
}

func TestOutcomesPageRepositoryErrorReturns500(t *testing.T) {
	opts := outcomesTestOptions(&fakeOutcomeRepo{conceptsErr: context.DeadlineExceeded})
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/outcomes", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestOutcomesPageIsReachableFromTheNav keeps the ninth topbar link and
// the route from drifting apart.
func TestOutcomesPageIsReachableFromTheNav(t *testing.T) {
	body := getOutcomes(t, outcomesTestOptions(&fakeOutcomeRepo{}))
	if !strings.Contains(body, `<a href="/outcomes">成果</a>`) {
		t.Errorf("topbar is missing the 成果 link: %s", body)
	}
}
