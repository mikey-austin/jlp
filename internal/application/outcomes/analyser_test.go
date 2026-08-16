package outcomes_test

import (
	"context"
	"errors"
	"fmt"
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

// steadyFading is the fading series of a learner who is actively
// writing: every one of n weeks carries runes characters of reviewed
// text. Classification tests use it because — since the fix for the
// "learner who stopped" defect — a concept can only be judged
// improving when there is evidence the learner actually produced
// something recently. A fixture with no writing at all is now the
// *inactive* case, and has its own tests below.
func steadyFading(n, runes, corrections int) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, n)
	for i := range out {
		out[i] = storage.WeeklyRate{
			WeekStart:   analyserNow.Add(-time.Duration(n-1-i) * 7 * day),
			Runes:       runes,
			Corrections: corrections,
		}
	}
	return out
}

// activeRepo is the common fixture: these concepts, belonging to a
// learner who is still writing.
func activeRepo(concepts ...storage.ConceptOutcome) *fakeOutcomeRepo {
	return &fakeOutcomeRepo{concepts: concepts, fading: steadyFading(8, 2000, 4)}
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
	repo := activeRepo(
		conceptOutcome("fewer", 120*day, 2*day, 9, 5, 4),
		conceptOutcome("equal", 120*day, 2*day, 9, 5, 5),
		conceptOutcome("more", 120*day, 2*day, 9, 5, 6),
		conceptOutcome("gone", 120*day, 2*day, 9, 5, 0),
	)
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
	repo := activeRepo(
		conceptOutcome("exactly-old-enough", minimum, day, 8, 5, 1),
		conceptOutcome("one-day-short", minimum-day, day, 8, 5, 1),
	)
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
	repo := activeRepo(
		conceptOutcome("one", 120*day, day, 4, 1, 0),
		conceptOutcome("two", 120*day, day, 4, 2, 0),
		conceptOutcome("three", 120*day, day, 4, 3, 0),
	)
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
	repo := activeRepo(
		// exactly 3 corrections, silent exactly 60 days: retired.
		conceptOutcome("exactly-retired", 200*day, 60*day, 3, 3, 0),
		// 3 corrections but one day short of the silence: still live.
		conceptOutcome("still-warm", 200*day, 60*day-day, 3, 3, 0),
		// silent for ages but only 2 corrections ever: not enough to
		// claim anything was learned, so excluded, never "retired".
		conceptOutcome("too-few", 200*day, 150*day, 2, 2, 0),
	)
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
	repo := activeRepo(conceptOutcome("slow-burn", 200*day, 90*day, 4, 1, 0))
	rep := report(t, repo)

	assertSlugs(t, "Retired", rep.Retired, "slow-burn")
	if len(rep.Excluded) != 0 {
		t.Errorf("Excluded = %+v, want none", rep.Excluded)
	}
}

// TestGroupsPreserveRepositoryOrder keeps the page's tables stable
// across reloads (the repository orders by slug).
func TestGroupsPreserveRepositoryOrder(t *testing.T) {
	repo := activeRepo(
		conceptOutcome("a", 120*day, day, 9, 5, 1),
		conceptOutcome("b", 120*day, day, 9, 5, 2),
		conceptOutcome("c", 120*day, day, 9, 5, 3),
	)
	assertSlugs(t, "Improving", report(t, repo).Improving, "a", "b", "c")
}

// ---------------------------------------------------------------------
// Inactivity is not improvement. These are the review's adversarial
// inputs, kept verbatim as the fixtures, because they are the exact
// scenarios that exposed the defect: a learner who simply stopped
// writing was being told, on every concept, that they had improved.
// ---------------------------------------------------------------------

// TestLearnerWhoStoppedWritingIsNeverCalledImproving is the review's
// case: five concepts, each last corrected 59 days ago, and NOTHING
// submitted for review in the entire 8-week window. Before the fix this
// reported "0 concepts retired, 5 improving, 0 still recurring."
//
// The package's own governing rule — silence is not evidence — applies
// to the recent side of the comparison exactly as it already applied to
// the baseline side: with no new writing, zero corrections measures
// nothing.
func TestLearnerWhoStoppedWritingIsNeverCalledImproving(t *testing.T) {
	concepts := make([]storage.ConceptOutcome, 0, 5)
	for _, slug := range []string{"c1", "c2", "c3", "c4", "c5"} {
		concepts = append(concepts, conceptOutcome(slug, 200*day, 59*day, 5, 5, 0))
	}
	repo := &fakeOutcomeRepo{concepts: concepts, fading: zeroFilledFading(8)}
	rep := report(t, repo)

	assertSlugs(t, "Improving", rep.Improving)
	assertSlugs(t, "Persistent", rep.Persistent)
	assertSlugs(t, "Retired", rep.Retired)
	if len(rep.Excluded) != 5 {
		t.Fatalf("Excluded = %d concepts, want all 5: %+v", len(rep.Excluded), rep.Excluded)
	}
	if !strings.Contains(rep.Excluded[0].Reason, "submitted for review in the last 30 days") {
		t.Errorf("Reason = %q, want it to name the missing recent writing", rep.Excluded[0].Reason)
	}

	// The sentence itself is asserted, not just the counts. Every one of
	// these concepts has 200 days of history — blaming their history
	// would be a different false claim in exactly the case this guard
	// exists for, and would contradict the page's own banner.
	want := "Less than 1,000 characters were submitted for review in the last 30 days (counted by whole weeks); " +
		"no concept could be judged yet; 5 concepts excluded for insufficient data."
	if rep.Headline != want {
		t.Fatalf("Headline =\n  %q\nwant\n  %q", rep.Headline, want)
	}
	if strings.Contains(rep.Headline, "history") {
		t.Errorf("headline blames history for concepts with 200 days of it: %q", rep.Headline)
	}
}

// TestRisingCorrectionsStayPersistentInALowVolumeWindow: the
// recent-writing gate exists to stop an ABSENCE of corrections reading
// as improvement. A concept whose corrections went UP is evidence
// regardless of how much was written — 9 corrections in 800 characters
// is not "no new writing", and withholding that judgement would be the
// opposite of the caution the gate was added for.
func TestRisingCorrectionsStayPersistentInALowVolumeWindow(t *testing.T) {
	fading := zeroFilledFading(8)
	fading[7].Runes = 800
	fading[7].Corrections = 9
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{
			conceptOutcome("worse", 200*day, day, 14, 5, 9),
			conceptOutcome("same", 200*day, day, 10, 5, 5),
			// Down, on the same thin volume: this one IS gated.
			conceptOutcome("down", 200*day, day, 6, 5, 1),
		},
		fading: fading,
	}
	rep := report(t, repo)

	assertSlugs(t, "Persistent", rep.Persistent, "worse", "same")
	assertSlugs(t, "Improving", rep.Improving)
	if len(rep.Excluded) != 1 || rep.Excluded[0].Slug != "down" {
		t.Fatalf("Excluded = %+v, want just the concept whose corrections fell", rep.Excluded)
	}
	// And the reason must not claim there was no writing when there was.
	if strings.Contains(rep.Excluded[0].Reason, "no new writing") {
		t.Errorf("Reason = %q asserts there was no writing, but 800 characters were reviewed", rep.Excluded[0].Reason)
	}
	if !strings.Contains(rep.Excluded[0].Reason, "only 800 characters") {
		t.Errorf("Reason = %q, want it to state the actual volume", rep.Excluded[0].Reason)
	}
}

// TestRecentWritingRequirementBoundary pins the new threshold on both
// sides, at exactly the minimum.
func TestRecentWritingRequirementBoundary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		runes     int
		improving []string
		excluded  int
	}{
		{"exactly at the minimum", outcomes.MinMeasurableRunes, []string{"c"}, 0},
		{"one character short", outcomes.MinMeasurableRunes - 1, nil, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// All the reviewed writing sits in the most recent week, so
			// it unambiguously falls inside the recent window.
			fading := zeroFilledFading(8)
			fading[7].Runes = tc.runes
			fading[7].Corrections = 1
			repo := &fakeOutcomeRepo{
				concepts: []storage.ConceptOutcome{conceptOutcome("c", 200*day, day, 9, 5, 1)},
				fading:   fading,
			}
			rep := report(t, repo)
			assertSlugs(t, "Improving", rep.Improving, tc.improving...)
			if len(rep.Excluded) != tc.excluded {
				t.Fatalf("Excluded = %+v, want %d", rep.Excluded, tc.excluded)
			}
		})
	}
}

// TestRecentWritingOutsideTheRecentWindowDoesNotCount: writing from
// three months ago is not evidence that the learner is producing now.
func TestRecentWritingOutsideTheRecentWindowDoesNotCount(t *testing.T) {
	fading := zeroFilledFading(8)
	fading[0].Runes = 50000 // the oldest of the 8 weeks — well outside 30 days
	fading[0].Corrections = 40
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("c", 200*day, day, 9, 5, 1)},
		fading:   fading,
	}
	rep := report(t, repo)
	assertSlugs(t, "Improving", rep.Improving)
	if len(rep.Excluded) != 1 {
		t.Fatalf("Excluded = %+v, want the concept excluded", rep.Excluded)
	}
}

// TestRetiredCountsCarryAnInactivityCaveat covers the review's second
// inactivity case: 12 concepts corrected three times each on one day
// long ago, learner gone since. "Retired" is what the brief specifies
// and it stays, but the headline must not state it as a finding about
// learning while omitting that nothing has been written.
func TestRetiredCountsCarryAnInactivityCaveat(t *testing.T) {
	concepts := make([]storage.ConceptOutcome, 0, 12)
	for i := 0; i < 12; i++ {
		concepts = append(concepts, conceptOutcome(fmt.Sprintf("r%02d", i), 61*day, 61*day, 3, 3, 0))
	}
	repo := &fakeOutcomeRepo{concepts: concepts, fading: zeroFilledFading(8)}

	want := "Less than 1,000 characters were submitted for review in the last 30 days (counted by whole weeks); " +
		"12 concepts retired, 0 improving, 0 still recurring."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline =\n  %q\nwant\n  %q", got, want)
	}
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
	// One measurable week (so no fading clause) but enough recent
	// writing that no inactivity caveat applies either — leaving the
	// concept counts alone in the sentence.
	fading := zeroFilledFading(8)
	fading[7].Runes = 10000
	fading[7].Corrections = 34
	repo := &fakeOutcomeRepo{
		concepts: []storage.ConceptOutcome{conceptOutcome("r1", 200*day, 90*day, 4, 4, 0)},
		fading:   fading,
	}
	want := "1 concept retired, 0 improving, 0 still recurring."
	if got := report(t, repo).Headline; got != want {
		t.Fatalf("Headline = %q, want %q", got, want)
	}
}

// ---------------------------------------------------------------------
// The fading clause needs a real denominator. Again the review's own
// adversarial inputs: a rate "per 1,000 characters" computed from five
// characters is not a rate.
// ---------------------------------------------------------------------

// TestFadingClauseIgnoresUndersizedWeeks is the review's five-character
// case. Before the fix this produced "fell from 200.0 to 3.4 over 8
// weeks" — a dramatic-sounding fall anchored on one clause of text.
func TestFadingClauseIgnoresUndersizedWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{
		fading: fadingFrom(
			[]int{5, 0, 0, 0, 0, 0, 0, 20000},
			[]int{1, 0, 0, 0, 0, 0, 0, 68}),
	}
	rep := report(t, repo)
	if strings.Contains(rep.Headline, "200.0") || strings.Contains(rep.Headline, "characters fell") {
		t.Fatalf("a 5-character week anchored a trend claim: %q", rep.Headline)
	}
	// One usable endpoint is not a trend, so the clause is dropped
	// entirely and — with no concepts either — nothing is claimed at all.
	if rep.Headline != "Not enough data yet to say whether corrections are improving later production." {
		t.Fatalf("Headline = %q", rep.Headline)
	}
	if rep.Fading[0].Measurable || rep.Fading[0].Per1000 != 0 {
		t.Errorf("undersized week marked measurable / given a rate: %+v", rep.Fading[0])
	}
	if !rep.Fading[7].Measurable {
		t.Errorf("20,000-character week not marked measurable: %+v", rep.Fading[7])
	}
}

// TestFadingClauseIgnoresTwoUndersizedWeeks is the review's second and
// worse case: 100 characters/1 correction then 120/0 previously
// produced "fell from 10.0 to 0.0" — and "0.0" is exactly the figure
// the zero-data guard forbids, because it reads as "no corrections
// needed".
func TestFadingClauseIgnoresTwoUndersizedWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{
		fading: fadingFrom(
			[]int{100, 0, 0, 0, 0, 0, 0, 120},
			[]int{1, 0, 0, 0, 0, 0, 0, 0}),
	}
	headline := report(t, repo).Headline
	for _, forbidden := range []string{"0.0", "10.0", "fell", "rose", "held steady"} {
		if strings.Contains(headline, forbidden) {
			t.Errorf("headline %q claims a trend from two sub-minimum samples (%q)", headline, forbidden)
		}
	}
}

// TestFadingClauseUsesTheFirstAndLastSUFFICIENT weeks: an undersized
// week at either end must not become an endpoint, and must not be
// counted in the span.
func TestFadingClauseUsesTheFirstAndLastSufficientWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{
		fading: fadingFrom(
			[]int{5, 10000, 0, 0, 0, 10000, 7, 0},
			[]int{1, 20, 0, 0, 0, 55, 3, 0}),
	}
	want := "Corrections per 1,000 characters rose from 2.0 to 5.5 over 5 weeks."
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
// TestHeadlineWhenEveryConceptIsExcluded: an actively-writing learner
// whose concepts are simply too new. The clause must not name a
// specific missing ingredient — the exclusions have several possible
// causes and the per-concept reasons in the table below state the
// actual one for each.
func TestHeadlineWhenEveryConceptIsExcluded(t *testing.T) {
	repo := activeRepo(
		conceptOutcome("x1", 10*day, day, 9, 5, 5),
		conceptOutcome("x2", 10*day, day, 9, 5, 5),
	)
	want := "No concept could be judged yet; 2 concepts excluded for insufficient data; " +
		"corrections per 1,000 characters held steady at 2.0 over 8 weeks."
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
	// The 1..5 scale spans 0..1 in FOUR steps, so a mean of 3.5 is
	// (3.5-1)/4 = 0.625 against a 0.75 correct rate: under-confident, so
	// the calibration error is negative.
	if got := rep.Calibration[0].Overconfidence; got > -0.1249 || got < -0.1251 {
		t.Errorf("Overconfidence = %v, want ~-0.125", got)
	}
	// A week nobody rated must divide by nothing and stay at zero — the
	// template renders Attempts == 0 as "no data", never as 0% and never
	// as perfectly calibrated.
	if rep.Calibration[1].MeanConfidence != 0 || rep.Calibration[1].CorrectRate != 0 || rep.Calibration[1].Overconfidence != 0 {
		t.Errorf("zero-attempt week derived non-zero rates: %+v", rep.Calibration[1])
	}
}

func TestDerivesFadingPer1000OnlyForSufficientWeeks(t *testing.T) {
	repo := &fakeOutcomeRepo{fading: []storage.WeeklyRate{
		{WeekStart: analyserNow.Add(-14 * day), Runes: 2000, Corrections: 6},
		{WeekStart: analyserNow.Add(-7 * day), Runes: 0, Corrections: 0},
		{WeekStart: analyserNow, Runes: 5, Corrections: 1},
	}}
	rep := report(t, repo)

	if !rep.Fading[0].Measurable || rep.Fading[0].Per1000 != 3.0 {
		t.Errorf("2,000-rune week = %+v, want measurable at 3.0", rep.Fading[0])
	}
	// Neither an empty week nor a five-character week gets a rate:
	// dividing 1,000 by a sample smaller than 1,000 is not a rate, it is
	// an extrapolation.
	for _, i := range []int{1, 2} {
		if rep.Fading[i].Measurable || rep.Fading[i].Per1000 != 0 {
			t.Errorf("week %d = %+v, want unmeasurable with no rate", i, rep.Fading[i])
		}
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
