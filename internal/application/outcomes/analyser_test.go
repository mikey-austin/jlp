package outcomes_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/outcomes"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// analyserNow is the fixed "now" every test below judges against — the
// Analyser never calls time.Now() itself (clock is injected), so every
// boundary case here is exact rather than "roughly 60 days".
var analyserNow = time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)

const day = 24 * time.Hour

// fakeOutcomeRepo is an in-memory storage.OutcomeRepository double
// returning exactly what a test configures — raw counts with every
// derived field (Retired, MeanConfidence, CorrectRate, Per1000) left
// zero, the same shape the real postgres repository returns.
type fakeOutcomeRepo struct {
	concepts    []storage.ConceptOutcome
	conceptsErr error

	calibration    []storage.CalibrationTrend
	calibrationErr error

	fading    []storage.WeeklyRate
	fadingErr error

	// gotWeeks records the weeks argument of the last CalibrationTrend
	// and AssistanceFading calls, so a test can pin the trend window.
	gotCalibrationWeeks int
	gotFadingWeeks      int
	gotIdentity         learner.IdentityID
}

func (f *fakeOutcomeRepo) ConceptOutcomes(_ context.Context, identity learner.IdentityID) ([]storage.ConceptOutcome, error) {
	f.gotIdentity = identity
	return f.concepts, f.conceptsErr
}

func (f *fakeOutcomeRepo) CalibrationTrend(_ context.Context, _ learner.IdentityID, weeks int) ([]storage.CalibrationTrend, error) {
	f.gotCalibrationWeeks = weeks
	return f.calibration, f.calibrationErr
}

func (f *fakeOutcomeRepo) AssistanceFading(_ context.Context, _ learner.IdentityID, weeks int) ([]storage.WeeklyRate, error) {
	f.gotFadingWeeks = weeks
	return f.fading, f.fadingErr
}

func newAnalyser(repo *fakeOutcomeRepo) *outcomes.Analyser {
	return outcomes.NewAnalyser(repo, func() time.Time { return analyserNow })
}

// conceptOutcome builds one raw ConceptOutcome positioned relative to
// analyserNow: firstAgo/lastAgo are how long before "now" the concept
// was first and last corrected.
func conceptOutcome(slug string, firstAgo, lastAgo time.Duration, total, before, after int) storage.ConceptOutcome {
	return storage.ConceptOutcome{
		Slug:              slug,
		Name:              strings.ToUpper(slug),
		FirstCorrectedAt:  analyserNow.Add(-firstAgo),
		LastCorrectedAt:   analyserNow.Add(-lastAgo),
		TotalCorrections:  total,
		CorrectionsBefore: before,
		CorrectionsAfter:  after,
	}
}

func report(t *testing.T, repo *fakeOutcomeRepo) outcomes.Report {
	t.Helper()
	rep, err := newAnalyser(repo).Report(context.Background(), "learner-a")
	if err != nil {
		t.Fatalf("Report returned error: %v", err)
	}
	return rep
}

// slugsOf is a readable assertion helper: which concepts landed in a
// group, in order.
func slugsOf(group []storage.ConceptOutcome) []string {
	out := make([]string, 0, len(group))
	for _, c := range group {
		out = append(out, c.Slug)
	}
	return out
}

func assertSlugs(t *testing.T, label string, got []storage.ConceptOutcome, want ...string) {
	t.Helper()
	gotSlugs := slugsOf(got)
	if strings.Join(gotSlugs, ",") != strings.Join(want, ",") {
		t.Errorf("%s = %v, want %v", label, gotSlugs, want)
	}
}

// ---------------------------------------------------------------------
// The zero-data case. This is what every brand-new learner sees, so it
// is pinned first and hardest: no group may contain anything, and the
// headline must not claim a rate, a percentage, or an improvement that
// does not exist.
// ---------------------------------------------------------------------

func TestReportWithNoDataSaysSoAndClaimsNothing(t *testing.T) {
	rep := report(t, &fakeOutcomeRepo{})

	if len(rep.Improving) != 0 || len(rep.Persistent) != 0 || len(rep.Retired) != 0 || len(rep.Excluded) != 0 {
		t.Fatalf("empty repository produced groups: %+v", rep)
	}
	want := "Not enough data yet to say whether corrections are improving later production."
	if rep.Headline != want {
		t.Fatalf("Headline = %q, want %q", rep.Headline, want)
	}
}

// TestZeroDataHeadlineNeverImpliesAZeroRate guards the specific lie the
// zero-data case is most likely to tell: a "0%" or "0.0" figure, or a
// "fell/rose" claim, when nothing has been measured at all.
func TestZeroDataHeadlineNeverImpliesAZeroRate(t *testing.T) {
	// Eight zero-filled weeks is exactly what the repository returns for
	// a brand-new learner — NOT an empty slice.
	repo := &fakeOutcomeRepo{
		fading:      zeroFilledFading(8),
		calibration: zeroFilledCalibration(8),
	}
	rep := report(t, repo)

	for _, forbidden := range []string{"0%", "0.0", "0 concept", "fell", "rose", "held steady"} {
		if strings.Contains(rep.Headline, forbidden) {
			t.Errorf("zero-data headline %q must not contain %q", rep.Headline, forbidden)
		}
	}
	if rep.Headline != "Not enough data yet to say whether corrections are improving later production." {
		t.Errorf("Headline = %q", rep.Headline)
	}
}

func zeroFilledFading(n int) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, n)
	for i := range out {
		out[i] = storage.WeeklyRate{WeekStart: analyserNow.Add(-time.Duration(n-1-i) * 7 * day)}
	}
	return out
}

func zeroFilledCalibration(n int) []storage.CalibrationTrend {
	out := make([]storage.CalibrationTrend, n)
	for i := range out {
		out[i] = storage.CalibrationTrend{WeekStart: analyserNow.Add(-time.Duration(n-1-i) * 7 * day)}
	}
	return out
}

// ---------------------------------------------------------------------
// Classification boundaries.
// ---------------------------------------------------------------------

// TestImprovingRequiresStrictlyFewerCorrectionsNow pins the improving /
// persistent boundary, including the exactly-equal case: equal is NOT
// improvement.
func TestImprovingRequiresStrictlyFewerCorrectionsNow(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("fewer", 120*day, 2*day, 9, 5, 4),
		conceptOutcome("equal", 120*day, 2*day, 9, 5, 5),
		conceptOutcome("more", 120*day, 2*day, 9, 5, 6),
		conceptOutcome("gone", 120*day, 2*day, 9, 5, 0),
	}}
	rep := report(t, repo)

	assertSlugs(t, "Improving", rep.Improving, "fewer", "gone")
	assertSlugs(t, "Persistent", rep.Persistent, "equal", "more")
	assertSlugs(t, "Retired", rep.Retired)
	if len(rep.Excluded) != 0 {
		t.Errorf("Excluded = %+v, want none", rep.Excluded)
	}
}

// TestInsufficientHistoryIsExcludedNotJudged pins the boundary that
// matters most: until a concept is OutcomeBaselineWindow +
// OutcomeRecentWindow old, its "before" and "after" windows overlap and
// comparing them is meaningless. Exactly-at-threshold is judged;
// one day short is excluded.
func TestInsufficientHistoryIsExcludedNotJudged(t *testing.T) {
	minimum := storage.OutcomeBaselineWindow + storage.OutcomeRecentWindow
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("exactly-old-enough", minimum, day, 8, 5, 1),
		conceptOutcome("one-day-short", minimum-day, day, 8, 5, 1),
	}}
	rep := report(t, repo)

	assertSlugs(t, "Improving", rep.Improving, "exactly-old-enough")
	if len(rep.Excluded) != 1 || rep.Excluded[0].Slug != "one-day-short" {
		t.Fatalf("Excluded = %+v, want just one-day-short", rep.Excluded)
	}
	if !strings.Contains(rep.Excluded[0].Reason, "59 days of history") {
		t.Errorf("Reason = %q, want it to state the actual history length", rep.Excluded[0].Reason)
	}
}

// TestThinBaselineIsExcludedNotJudged: a concept whose baseline window
// holds only one or two corrections has no rate to compare against —
// a drop to zero there is noise, not evidence. Exactly three is enough.
func TestThinBaselineIsExcludedNotJudged(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("one", 120*day, day, 4, 1, 0),
		conceptOutcome("two", 120*day, day, 4, 2, 0),
		conceptOutcome("three", 120*day, day, 4, 3, 0),
	}}
	rep := report(t, repo)

	assertSlugs(t, "Improving", rep.Improving, "three")
	if len(rep.Excluded) != 2 {
		t.Fatalf("Excluded = %+v, want one and two", rep.Excluded)
	}
	assertSlugs(t, "Excluded", []storage.ConceptOutcome{rep.Excluded[0].ConceptOutcome, rep.Excluded[1].ConceptOutcome}, "one", "two")
	if !strings.Contains(rep.Excluded[0].Reason, "1 correction") {
		t.Errorf("Reason = %q, want it to state the actual baseline size", rep.Excluded[0].Reason)
	}
}

// TestRetiredNeedsBothThreePriorCorrectionsAndSixtyDaySilence pins both
// halves of the retired rule at their exact thresholds.
func TestRetiredNeedsBothThreePriorCorrectionsAndSixtyDaySilence(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		// exactly 3 corrections, silent exactly 60 days: retired.
		conceptOutcome("exactly-retired", 200*day, 60*day, 3, 3, 0),
		// 3 corrections but one day short of the silence: still live.
		conceptOutcome("still-warm", 200*day, 60*day-day, 3, 3, 0),
		// silent for ages but only 2 corrections ever: not enough to
		// claim anything was learned, so excluded, never "retired".
		conceptOutcome("too-few", 200*day, 150*day, 2, 2, 0),
	}}
	rep := report(t, repo)

	assertSlugs(t, "Retired", rep.Retired, "exactly-retired")
	assertSlugs(t, "Improving", rep.Improving, "still-warm")
	if len(rep.Excluded) != 1 || rep.Excluded[0].Slug != "too-few" {
		t.Fatalf("Excluded = %+v, want just too-few", rep.Excluded)
	}
	for _, c := range rep.Retired {
		if !c.Retired {
			t.Errorf("%s is in Retired but its Retired flag is false", c.Slug)
		}
	}
	for _, c := range append(append([]storage.ConceptOutcome{}, rep.Improving...), rep.Persistent...) {
		if c.Retired {
			t.Errorf("%s is not retired but carries Retired=true", c.Slug)
		}
	}
}

// TestRetiredOutranksAThinBaseline: "≥3 corrections then 60 days of
// silence" stands on its own — it never needed the before/after rate
// comparison, so a thin baseline must not demote it to excluded.
func TestRetiredOutranksAThinBaseline(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("slow-burn", 200*day, 90*day, 4, 1, 0),
	}}
	rep := report(t, repo)

	assertSlugs(t, "Retired", rep.Retired, "slow-burn")
	if len(rep.Excluded) != 0 {
		t.Errorf("Excluded = %+v, want none", rep.Excluded)
	}
}

// TestGroupsPreserveRepositoryOrder keeps the page's tables stable
// across reloads (the repository orders by slug).
func TestGroupsPreserveRepositoryOrder(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("a", 120*day, day, 9, 5, 1),
		conceptOutcome("b", 120*day, day, 9, 5, 2),
		conceptOutcome("c", 120*day, day, 9, 5, 3),
	}}
	assertSlugs(t, "Improving", report(t, repo).Improving, "a", "b", "c")
}

// ---------------------------------------------------------------------
// Headline.
// ---------------------------------------------------------------------

// TestHeadlineStatesEveryGroupAndTheFadingTrend pins the exact sentence
// shape from the task brief — plain fact, no score, no streak, no
// encouragement.
func TestHeadlineStatesEveryGroupAndTheFadingTrend(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{
			conceptOutcome("r1", 200*day, 90*day, 4, 4, 0),
			conceptOutcome("r2", 200*day, 90*day, 4, 4, 0),
			conceptOutcome("r3", 200*day, 90*day, 4, 4, 0),
			conceptOutcome("i1", 120*day, day, 9, 5, 1),
			conceptOutcome("p1", 120*day, day, 9, 5, 5),
			conceptOutcome("p2", 120*day, day, 9, 5, 6),
			conceptOutcome("x1", 10*day, day, 9, 5, 5),
			conceptOutcome("x2", 10*day, day, 9, 5, 5),
		},
		fading: fadingFrom([]int{10000, 0, 0, 0, 0, 0, 0, 10000}, []int{81, 0, 0, 0, 0, 0, 0, 34}),
	}
	rep := report(t, repo)

	want := "3 concepts retired, 1 improving, 2 still recurring, 2 excluded for insufficient data; " +
		"corrections per 1,000 characters fell from 8.1 to 3.4 over 8 weeks."
	if rep.Headline != want {
		t.Fatalf("Headline =\n  %q\nwant\n  %q", rep.Headline, want)
	}
}

// fadingFrom builds a zero-filled 8-week fading slice from per-week
// runes and correction counts.
func fadingFrom(runes, corrections []int) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, len(runes))
	for i := range runes {
		out[i] = storage.WeeklyRate{
			WeekStart:   analyserNow.Add(-time.Duration(len(runes)-1-i) * 7 * day),
			Runes:       runes[i],
			Corrections: corrections[i],
		}
	}
	return out
}

func TestHeadlineUsesSingularNounForOneConcept(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("r1", 200*day, 90*day, 4, 4, 0),
	}}
	want := "1 concept retired, 0 improving, 0 still recurring."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// TestHeadlineSaysHeldSteadyRatherThanFellFromXToX: the two endpoints
// are compared as displayed, so the sentence never reads "fell from 3.4
// to 3.4".
func TestHeadlineSaysHeldSteadyRatherThanFellFromXToX(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("i1", 120*day, day, 9, 5, 1)},
		fading:   fadingFrom([]int{10000, 0, 0, 0, 0, 0, 0, 20000}, []int{34, 0, 0, 0, 0, 0, 0, 68}),
	}
	want := "0 concepts retired, 1 improving, 0 still recurring; " +
		"corrections per 1,000 characters held steady at 3.4 over 8 weeks."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

func TestHeadlineSaysRoseWhenTheRateWentUp(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("p1", 120*day, day, 9, 5, 9)},
		fading:   fadingFrom([]int{10000, 0, 0, 0, 0, 10000, 0, 0}, []int{20, 0, 0, 0, 0, 55, 0, 0}),
	}
	// Span is measured between the FIRST and LAST week that actually had
	// text to measure — weeks 1..6 here, not the full 8.
	want := "0 concepts retired, 0 improving, 1 still recurring; " +
		"corrections per 1,000 characters rose from 2.0 to 5.5 over 6 weeks."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// TestHeadlineOmitsTheFadingClauseWithOnlyOneMeasurableWeek: one week
// is a point, not a trend, and a trend claimed from one point is a lie.
func TestHeadlineOmitsTheFadingClauseWithOnlyOneMeasurableWeek(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("i1", 120*day, day, 9, 5, 1)},
		fading:   fadingFrom([]int{0, 0, 0, 0, 0, 0, 0, 10000}, []int{0, 0, 0, 0, 0, 0, 0, 34}),
	}
	want := "0 concepts retired, 1 improving, 0 still recurring."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// TestHeadlineWhenEveryConceptIsExcluded must not report "0 improving"
// as though that were a finding — nothing was judged at all.
func TestHeadlineWhenEveryConceptIsExcluded(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("x1", 10*day, day, 9, 5, 5),
		conceptOutcome("x2", 10*day, day, 9, 5, 5),
	}}
	want := "No concept has enough history to judge yet; 2 concepts excluded for insufficient data."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// TestHeadlineReportsFadingEvenWithNoJudgeableConcepts: the two halves
// are independent facts.
func TestHeadlineReportsFadingEvenWithNoJudgeableConcepts(t *testing.T) {
	repo := &fakeOutcomeRepo{
		fading: fadingFrom([]int{10000, 0, 0, 0, 0, 0, 0, 10000}, []int{81, 0, 0, 0, 0, 0, 0, 34}),
	}
	want := "Corrections per 1,000 characters fell from 8.1 to 3.4 over 8 weeks."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// TestHeadlineIsNeverAScoreOrAStreak is a standing guard against PRD
// §56 gamification creeping into the one sentence a learner is most
// likely to read.
func TestHeadlineIsNeverAScoreOrAStreak(t *testing.T) {
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("i1", 120*day, day, 9, 5, 1)},
		fading:   fadingFrom([]int{1000, 0, 0, 0, 0, 0, 0, 1000}, []int{81, 0, 0, 0, 0, 0, 0, 34}),
	}
	headline := report(t, repo).Headline
	for _, forbidden := range []string{
		"streak", "score", "点", "Great", "well done", "keep it up", "!", "🎉", "level", "badge",
	} {
		if strings.Contains(strings.ToLower(headline), strings.ToLower(forbidden)) {
			t.Errorf("headline %q contains gamification token %q", headline, forbidden)
		}
	}
}

// ---------------------------------------------------------------------
// Derived ratios and plumbing.
// ---------------------------------------------------------------------

func TestDerivesCalibrationRatesIncludingZeroAttemptWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{calibration: []storage.CalibrationTrend{
		{WeekStart: analyserNow.Add(-7 * day), Attempts: 4, ConfidenceTotal: 14, Corrects: 3},
		{WeekStart: analyserNow, Attempts: 0},
	}}
	rep := report(t, repo)

	if len(rep.Calibration) != 2 {
		t.Fatalf("Calibration = %+v", rep.Calibration)
	}
	if rep.Calibration[0].MeanConfidence != 3.5 {
		t.Errorf("MeanConfidence = %v, want 3.5", rep.Calibration[0].MeanConfidence)
	}
	if rep.Calibration[0].CorrectRate != 0.75 {
		t.Errorf("CorrectRate = %v, want 0.75", rep.Calibration[0].CorrectRate)
	}
	// 3.5/5 = 0.70 confident against 0.75 correct: slightly UNDER
	// confident, so the calibration error is negative.
	if got := rep.Calibration[0].Overconfidence; got > -0.049 || got < -0.051 {
		t.Errorf("Overconfidence = %v, want ~-0.05", got)
	}
	// A week nobody rated must divide by nothing and stay at zero — the
	// template renders Attempts == 0 as "no data", never as 0% and never
	// as perfectly calibrated.
	if rep.Calibration[1].MeanConfidence != 0 || rep.Calibration[1].CorrectRate != 0 || rep.Calibration[1].Overconfidence != 0 {
		t.Errorf("zero-attempt week derived non-zero rates: %+v", rep.Calibration[1])
	}
}

func TestDerivesFadingPer1000IncludingZeroRuneWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{fading: []storage.WeeklyRate{
		{WeekStart: analyserNow.Add(-7 * day), Runes: 2000, Corrections: 6},
		{WeekStart: analyserNow, Runes: 0, Corrections: 0},
	}}
	rep := report(t, repo)

	if rep.Fading[0].Per1000 != 3.0 {
		t.Errorf("Per1000 = %v, want 3.0", rep.Fading[0].Per1000)
	}
	if rep.Fading[1].Per1000 != 0 {
		t.Errorf("zero-rune week Per1000 = %v, want 0", rep.Fading[1].Per1000)
	}
}

func TestReportAsksForTheTrendWindowAndScopesToIdentity(t *testing.T) {
	repo := &fakeOutcomeRepo{}
	if _, err := newAnalyser(repo).Report(context.Background(), "learner-b"); err != nil {
		t.Fatal(err)
	}
	if repo.gotCalibrationWeeks != outcomes.TrendWeeks || repo.gotFadingWeeks != outcomes.TrendWeeks {
		t.Errorf("weeks = %d/%d, want %d", repo.gotCalibrationWeeks, repo.gotFadingWeeks, outcomes.TrendWeeks)
	}
	if repo.gotIdentity != learner.IdentityID("learner-b") {
		t.Errorf("identity = %q", repo.gotIdentity)
	}
}

func TestReportPropagatesRepositoryErrors(t *testing.T) {
	boom := errors.New("boom")
	for name, repo := range map[string]*fakeOutcomeRepo{
		"concepts":    {conceptsErr: boom},
		"calibration": {calibrationErr: boom},
		"fading":      {fadingErr: boom},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := newAnalyser(repo).Report(context.Background(), "learner-a"); !errors.Is(err, boom) {
				t.Fatalf("err = %v, want %v", err, boom)
			}
		})
	}
}

// TestReportDoesNotMutateTheRepositorySlice guards against the analyser
// classifying by shuffling the caller's backing array.
func TestReportDoesNotMutateTheRepositorySlice(t *testing.T) {
	repo := &fakeOutcomeRepo{concepts: []storage.ConceptOutcome{
		conceptOutcome("a", 120*day, day, 9, 5, 1),
		conceptOutcome("b", 10*day, day, 9, 5, 1),
	}}
	report(t, repo)
	if repo.concepts[0].Slug != "a" || repo.concepts[1].Slug != "b" {
		t.Fatalf("repository slice reordered: %v", slugsOf(repo.concepts))
	}
	if repo.concepts[0].Retired {
		t.Error("analyser wrote a derived judgement back into the repository's slice")
	}
}
