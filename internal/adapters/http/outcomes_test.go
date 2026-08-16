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

// weeklyFading builds a full TrendWeeks-long fading series from
// per-week runes and correction counts — the shape the real repository
// always returns (it zero-fills to exactly the requested week count),
// so no test here feeds a slice production could not produce.
func weeklyFading(runes, corrections []int) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, len(runes))
	for i := range runes {
		out[i] = storage.WeeklyRate{
			WeekStart:   outcomesNow.Add(-time.Duration(len(runes)-1-i) * 7 * outcomesDay),
			Runes:       runes[i],
			Corrections: corrections[i],
		}
	}
	return out
}

// activeFading is a learner who is still writing — enough reviewed text
// in the recent window that concepts may be judged at all. Page tests
// that are about rendering, not about the inactivity guard, use it.
func activeFading() []storage.WeeklyRate {
	return weeklyFading(
		[]int{4000, 4000, 4000, 4000, 4000, 4000, 4000, 4000},
		[]int{12, 12, 12, 12, 12, 12, 12, 12})
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
	}, fading: activeFading()}
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
	if !strings.Contains(body, "1 concept retired, 1 improving, 1 still recurring, 2 excluded for insufficient data;") {
		t.Errorf("headline missing or wrong: %s", body)
	}
}

// TestOutcomesPageDisclosesWhatItCannotSee: conversation and spoken
// practice corrections live as jsonb on conversation_turns, not in the
// corrections table, so no number on this page can see them. A page
// whose whole claim is honesty must say that — especially since a
// learner who moves to conversation practice would otherwise look like
// they improved.
func TestOutcomesPageDisclosesWhatItCannotSee(t *testing.T) {
	body := getOutcomes(t, outcomesTestOptions(&fakeOutcomeRepo{fading: activeFading()}))
	for _, want := range []string{"添削に出した文章だけ", "会話練習", "音声練習"} {
		if !strings.Contains(body, want) {
			t.Errorf("/outcomes does not disclose that %q is out of scope: %s", want, body)
		}
	}
}

// TestOutcomesPageWarnsWhenTheLearnerHasBarelyWritten is the page-level
// half of the "inactivity is not improvement" fix: the caveat must be
// on the page itself, not only inside the headline sentence.
func TestOutcomesPageWarnsWhenTheLearnerHasBarelyWritten(t *testing.T) {
	quiet := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{outcomeRow("gone-quiet", 200*outcomesDay, 61*outcomesDay, 4, 4, 0)},
		fading:   weeklyFading([]int{0, 0, 0, 0, 0, 0, 0, 0}, []int{0, 0, 0, 0, 0, 0, 0, 0}),
	}
	body := getOutcomes(t, outcomesTestOptions(quiet))
	if !strings.Contains(body, "に添削へ出した文章は0文字です。") {
		t.Errorf("no inactivity caveat on a page with no recent writing: %s", body)
	}
	// The banner must not claim a literal 30-day span: RecentRunes is
	// summed from weekly buckets, so the straddling week counts in full.
	if !strings.Contains(body, "週単位で集計するため最大6日分多く含みます") {
		t.Errorf("banner claims a literal 30-day window it does not measure: %s", body)
	}
	if !strings.Contains(body, "Less than 1,000 characters were submitted for review in the last 30 days (counted by whole weeks);") {
		t.Errorf("headline does not lead with the inactivity caveat: %s", body)
	}

	// ...and it is absent when the learner is writing.
	active := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{outcomeRow("improving-slug", 120*outcomesDay, outcomesDay, 9, 5, 1)},
		fading:   activeFading(),
	}
	if body := getOutcomes(t, outcomesTestOptions(active)); strings.Contains(body, "に添削へ出した文章は") {
		t.Errorf("inactivity caveat shown to an active learner: %s", body)
	}
}

// TestOutcomesPageDoesNotPlotUnmeasuredWeeksAtTheChartFloor: a week
// with nothing submitted has no rate, and 0 is the BOTTOM of the axis
// — plotting it would draw a gap as "corrections fell to zero".
func TestOutcomesPageDoesNotPlotUnmeasuredWeeksAtTheChartFloor(t *testing.T) {
	repo := &fakeOutcomeRepo{fading: weeklyFading(
		[]int{10000, 0, 0, 0, 0, 0, 0, 10000},
		[]int{81, 0, 0, 0, 0, 0, 0, 34})}
	body := getOutcomes(t, outcomesTestOptions(repo))

	// Two measurable weeks at the two ends of an 8-week axis: exactly
	// two points, at x=0 and x=120, and nothing in between. The six
	// unmeasured weeks contribute no coordinate at all.
	if !strings.Contains(body, `<polyline points="0.0,2.0 120.0,13.6" />`) {
		t.Errorf("sparkline plots weeks it has no value for: %s", body)
	}
	// 22.0 is the chart floor (pad 2 + plot height 20). Nothing may sit
	// there: no week in this series has the minimum rate of zero.
	if strings.Contains(body, "22.0") {
		t.Errorf("an unmeasured week was plotted at the chart floor: %s", body)
	}
}

// TestOutcomesPageGivesNoRateToUndersizedWeeks: the table still shows
// the raw runes and corrections (facts), but withholds the per-1,000
// rate, which a sub-1,000-character sample cannot support.
func TestOutcomesPageGivesNoRateToUndersizedWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{fading: weeklyFading(
		[]int{10000, 5, 0, 0, 0, 0, 0, 10000},
		[]int{81, 1, 0, 0, 0, 0, 0, 34})}
	body := getOutcomes(t, outcomesTestOptions(repo))

	if strings.Contains(body, "200.0") {
		t.Errorf("a 5-character week was given a per-1,000 rate: %s", body)
	}
	// Its raw counts are still there — they are facts, and only the
	// derived rate is unsupported.
	if !strings.Contains(body, `<td data-label="添削した文字数">5</td>`) {
		t.Errorf("the undersized week's raw character count was hidden: %s", body)
	}
}

// TestOutcomesPageStillShowsCountsWhenNoWeekCarriesARate: a learner who
// submitted a little text every week, but never 1,000 characters in one
// week, has no rate anywhere — and must still see what they actually
// wrote. Only the derived figure is withheld, never the measurement.
// (This is the dev database's own shape: 632 reviewed characters across
// the window, which before this fix was reported as "45.9 corrections
// per 1,000 characters".)
func TestOutcomesPageStillShowsCountsWhenNoWeekCarriesARate(t *testing.T) {
	repo := &fakeOutcomeRepo{fading: weeklyFading(
		[]int{0, 0, 0, 0, 0, 0, 300, 332},
		[]int{0, 0, 0, 0, 0, 0, 12, 17})}
	body := getOutcomes(t, outcomesTestOptions(repo))

	if strings.Contains(body, `<polyline`) {
		t.Errorf("drew a trend with no week carrying a rate: %s", body)
	}
	for _, want := range []string{
		`<td data-label="添削した文字数">300</td>`,
		`<td data-label="添削した文字数">332</td>`,
		`<td data-label="修正数">17</td>`,
		`<td data-label="修正/1000字">—</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("fading table missing %q: %s", want, body)
		}
	}
}

// TestOutcomesPageDrawsTheFadingTrendOnlyWithTwoMeasurableWeeks pins
// the chart's own honesty rule, on both sides of the boundary.
func TestOutcomesPageDrawsTheFadingTrendOnlyWithTwoMeasurableWeeks(t *testing.T) {
	oneWeek := &fakeOutcomeRepo{fading: weeklyFading(
		[]int{0, 0, 0, 0, 0, 0, 0, 10000},
		[]int{0, 0, 0, 0, 0, 0, 0, 34})}
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

	twoWeeks := &fakeOutcomeRepo{fading: weeklyFading(
		[]int{0, 0, 0, 0, 0, 0, 10000, 10000},
		[]int{0, 0, 0, 0, 0, 0, 81, 34})}
	body = getOutcomes(t, outcomesTestOptions(twoWeeks))
	if !strings.Contains(body, `<polyline`) {
		t.Errorf("did not draw a trend from two measurable weeks: %s", body)
	}
	for _, want := range []string{
		"Corrections per 1,000 characters fell from 8.1 to 3.4 over 2 weeks.",
		"8.1", "3.4", "修正/1000字",
		"直近8週のうち計測できた週：2",
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
		fading: weeklyFading(
			[]int{0, 0, 0, 0, 0, 10000, 0, 10000},
			[]int{0, 0, 0, 0, 0, 81, 0, 34}),
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
	// 16/4 = 4.0 mean confidence, 2/4 = 50% correct. The 1..5 scale maps
	// onto 0..1 in four steps, so (4.0-1)/4 = 0.75 against 0.50 = +25pt.
	for _, want := range []string{"4.0 / 5", "50%", "25pt"} {
		if !strings.Contains(body, want) {
			t.Errorf("calibration row missing %q: %s", want, body)
		}
	}
}

// TestOutcomesPageHasNoGamification is a standing PRD §56 guard on the
// rendered page, not just on the headline string.
func TestOutcomesPageHasNoGamification(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{outcomeRow("improving-slug", 120*outcomesDay, outcomesDay, 9, 5, 1)},
		fading:   activeFading(),
	}
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
