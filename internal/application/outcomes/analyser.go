// Package outcomes answers the one question the whole product exists to
// answer (PRD §52/§72/§73): did being corrected lead to better later
// production?
//
// It is deliberately the only place in the codebase that turns raw
// correction counts into a judgement. storage.OutcomeRepository returns
// nothing but counts and timestamps; every threshold that decides
// "improving" from "persistent" from "retired" from "not enough data to
// say" lives here, in plain Go, where a unit test can walk each
// boundary exactly (see analyser_test.go). That split is the project
// convention — SQL returns raw counts, the application layer derives
// judgements — and it matters more here than anywhere else: a
// threshold buried in SQL is a confident claim about somebody's
// learning that nothing can check.
//
// Three rules govern everything below.
//
//  1. Silence is not evidence — on BOTH sides of the comparison. A
//     concept whose two measurement windows still overlap, or whose
//     baseline holds one or two corrections, or whose recent window
//     contains no meaningful amount of reviewed writing, is EXCLUDED —
//     not "improving", not "persistent". The third of those is the one
//     that matters most: a learner who simply stopped writing has zero
//     corrections everywhere, and calling that improvement would be
//     telling someone who quit that they got better. The Report carries
//     every exclusion with a per-concept reason so the page can say how
//     many were dropped and why, rather than quietly shrinking the
//     denominator.
//  2. The headline is a fact, never a verdict. It states counts and, if
//     and only if there are at least two measurable weeks, the direction
//     the assistance rate moved. It contains no score, no streak, no
//     percentage-improved, and no encouragement (PRD §56).
//  3. Nothing at all measured means exactly that. Report on an empty
//     history produces "Not enough data yet…" — never "0% improvement",
//     which reads as a measured failure.
package outcomes

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// TrendWeeks is how many trailing ISO weeks the calibration and
// assistance-fading trends cover — the same 8-week window the home
// dashboard's weakness sparklines use, so the two pages' x-axes mean
// the same thing.
const TrendWeeks = 8

const (
	// minBaselineCorrections is how many corrections a concept's
	// baseline window must hold before its "after" count is worth
	// comparing against. With one or two, a drop to zero is
	// indistinguishable from the learner simply not having written
	// about that concept lately — which is why such concepts are
	// excluded rather than counted as improvements. Three is the
	// smallest count where "it used to keep happening" is a fair
	// description.
	minBaselineCorrections = 3

	// retiredSilence is how long a concept must go uncorrected before
	// it is called retired, and minRetiredCorrections is how many
	// corrections must have preceded that silence. Both halves are
	// required: 60 quiet days after a single correction says nothing
	// (the learner may simply never have used the form again), whereas
	// 60 quiet days after three or more says the pattern stopped.
	retiredSilence        = 60 * 24 * time.Hour
	minRetiredCorrections = 3
)

// MinMeasurableRunes is the smallest amount of reviewed writing from
// which anything at all may be said about a correction rate. It is
// exported so tests can pin the boundary at exactly the threshold
// rather than restating the number.
//
// It is not a taste judgement: the rate this package reports is
// "corrections per 1,000 characters", quoted to one decimal place. In a
// sample SMALLER than 1,000 characters, a single correction moves that
// figure by more than 1.0 — the quoted decimal is then pure noise, and
// the headline ends up saying things like "fell from 200.0 to 3.4"
// off the back of one reviewed clause. Requiring the denominator to be
// at least as large as the unit the rate is quoted in is the smallest
// floor that makes the number mean what it says.
//
// The same constant does two jobs, deliberately, because it is the same
// question in both:
//
//   - a week must clear it before it can be an endpoint of the fading
//     trend (or be plotted, or be given a rate in the table), and
//   - the RECENT window must clear it before any concept may be judged
//     improving or persistent — otherwise "fewer corrections" is just
//     "less writing".
const MinMeasurableRunes = 1000

// weekSpan is how much time one storage.WeeklyRate bucket covers. Used
// only to decide which buckets overlap the recent window.
const weekSpan = 7 * 24 * time.Hour

// minimumHistory is how old a concept's first correction must be before
// its baseline and recent windows stop overlapping — the point at which
// comparing CorrectionsBefore with CorrectionsAfter measures a CHANGE
// rather than double-counting the same corrections at both ends. It is
// derived from the port's own window definitions rather than restated,
// so widening either window automatically widens this.
const minimumHistory = storage.OutcomeBaselineWindow + storage.OutcomeRecentWindow

// Excluded is one concept that could not honestly be judged, with the
// reason stated in the learner's terms. The embedded ConceptOutcome
// keeps Slug/Name/counts directly addressable from a template, exactly
// as the three judged groups are.
type Excluded struct {
	storage.ConceptOutcome
	// Reason states what is missing — the actual history length or the
	// actual baseline size, not a generic "insufficient data".
	Reason string
}

// Report is everything the /outcomes page renders. The three judged
// groups partition the concepts that COULD be judged; Excluded holds
// the rest, and Improving+Persistent+Retired+Excluded always accounts
// for every concept the repository returned — no concept is ever
// silently dropped.
type Report struct {
	// Improving: fewer corrections in the recent window than in the
	// baseline window.
	Improving []storage.ConceptOutcome
	// Persistent: the same number or more. "Same" is deliberately
	// persistent rather than improving — no change is no evidence of
	// change.
	Persistent []storage.ConceptOutcome
	// Retired: at least minRetiredCorrections corrections, then
	// retiredSilence with none. Checked before everything else: it is a
	// self-contained claim that never needed the before/after
	// comparison, so a thin baseline must not demote it.
	Retired []storage.ConceptOutcome
	// Excluded: concepts with too little history or too thin a
	// baseline, each with its reason.
	Excluded []Excluded
	// Calibration and Fading are the two weekly trends, zero-filled to
	// exactly TrendWeeks entries by the repository, with their derived
	// ratios filled in here.
	Calibration []storage.CalibrationTrend
	Fading      []storage.WeeklyRate
	// RecentRunes is how many characters were submitted for review
	// inside the recent window — the evidence that the learner is still
	// producing anything at all. Below MinMeasurableRunes no concept is
	// judged improving or persistent, and the headline says so.
	RecentRunes int
	// Headline is one sentence of plain fact. See the package doc
	// comment's rule 2, and headline() below.
	Headline string
}

// Analyser derives a Report from raw repository counts. It never calls
// time.Now() itself — clock is injected, the same way
// application/retrieval.Scheduler and application/learnermodel.Updater
// take theirs, so every boundary in analyser_test.go is exact rather
// than relative to whenever the suite happens to run.
type Analyser struct {
	repo  storage.OutcomeRepository
	clock func() time.Time
}

// NewAnalyser builds an Analyser over repo. Production wiring passes
// time.Now (see cmd/jlp/main.go).
func NewAnalyser(repo storage.OutcomeRepository, clock func() time.Time) *Analyser {
	return &Analyser{repo: repo, clock: clock}
}

// Report reads identity's raw outcome evidence and classifies it.
func (a *Analyser) Report(ctx context.Context, identity learner.IdentityID) (Report, error) {
	concepts, err := a.repo.ConceptOutcomes(ctx, identity)
	if err != nil {
		return Report{}, fmt.Errorf("outcomes: concept outcomes: %w", err)
	}
	calibration, err := a.repo.CalibrationTrend(ctx, identity, TrendWeeks)
	if err != nil {
		return Report{}, fmt.Errorf("outcomes: calibration trend: %w", err)
	}
	fading, err := a.repo.AssistanceFading(ctx, identity, TrendWeeks)
	if err != nil {
		return Report{}, fmt.Errorf("outcomes: assistance fading: %w", err)
	}

	now := a.clock()
	rep := Report{
		Calibration: deriveCalibration(calibration),
		Fading:      deriveFading(fading),
	}
	// Computed BEFORE classification, and consulted by it: the fading
	// data the analyser already holds is exactly the evidence that
	// disproves "they improved" for a learner who stopped writing.
	rep.RecentRunes = recentReviewedRunes(rep.Fading, now)

	for _, c := range concepts {
		// Copied by value before anything is written to it: the
		// repository's slice is the caller's, and Retired below is a
		// judgement this package owns, not data the repository handed
		// over.
		outcome := c
		switch group, reason := classify(outcome, now, rep.RecentRunes); group {
		case groupRetired:
			outcome.Retired = true
			rep.Retired = append(rep.Retired, outcome)
		case groupImproving:
			rep.Improving = append(rep.Improving, outcome)
		case groupPersistent:
			rep.Persistent = append(rep.Persistent, outcome)
		default:
			rep.Excluded = append(rep.Excluded, Excluded{ConceptOutcome: outcome, Reason: reason})
		}
	}
	rep.Headline = headline(rep)
	return rep, nil
}

type group int

const (
	groupExcluded group = iota
	groupRetired
	groupImproving
	groupPersistent
)

// classify applies the three rules in the only order that is defensible:
//
//  1. Retired first, because it is self-contained — "three or more
//     corrections, then sixty silent days" needs no window comparison,
//     so neither a short history (impossible: sixty silent days implies
//     at least sixty days of history) nor a thin baseline can refute it.
//  2. Then the three exclusions, because a concept that cannot be
//     measured must never fall through into a judged group.
//  3. Only then the before/after comparison.
//
// recentRunes is how much reviewed writing the learner produced inside
// the recent window. It gates ONLY the improving arm, and that
// asymmetry is the whole point rather than an oversight:
//
//   - Fewer corrections than before is evidence of improvement only if
//     there was something to be corrected. With almost no new writing,
//     a drop to zero is what quitting looks like, so the judgement is
//     withheld — the same argument minBaselineCorrections makes about
//     the baseline side.
//   - As many corrections as before, or more, is evidence on its own
//     terms. Nine corrections inside 800 characters is not "no new
//     writing", and the corrections went UP; suppressing that would
//     withhold a well-supported finding in the name of caution, which
//     is the opposite of what the caution is for. Low volume can make
//     a persistent verdict UNDERSTATE the problem — never overstate it
//     — so it is safe to report.
//
// The returned reason is non-empty only for groupExcluded.
func classify(c storage.ConceptOutcome, now time.Time, recentRunes int) (group, string) {
	if c.TotalCorrections >= minRetiredCorrections && !c.LastCorrectedAt.After(now.Add(-retiredSilence)) {
		return groupRetired, ""
	}

	history := now.Sub(c.FirstCorrectedAt)
	if history < minimumHistory {
		return groupExcluded, fmt.Sprintf(
			"only %d days of history — the before and after windows still overlap below %d days",
			int(history/(24*time.Hour)), int(minimumHistory/(24*time.Hour)))
	}
	if c.CorrectionsBefore < minBaselineCorrections {
		return groupExcluded, fmt.Sprintf(
			"only %s in the first %d days — too few to compare a later rate against",
			countOf(c.CorrectionsBefore, "correction"), int(storage.OutcomeBaselineWindow/(24*time.Hour)))
	}
	if c.CorrectionsAfter < c.CorrectionsBefore {
		if recentRunes < MinMeasurableRunes {
			return groupExcluded, fmt.Sprintf(
				"only %d characters submitted for review in the last %d days — too little new writing for fewer corrections to mean anything",
				recentRunes, int(storage.OutcomeRecentWindow/(24*time.Hour)))
		}
		return groupImproving, ""
	}
	return groupPersistent, ""
}

// confidenceScaleMin/Max are the ends of the self-reported confidence
// scale, fixed by the exercise_attempts_confidence_range CHECK
// constraint (1..5) in the 00013 migration. They exist only to put
// MeanConfidence on the same 0..1 scale as CorrectRate so the two can
// be subtracted.
//
// The mapping is (mean - min) / (max - min), NOT mean / max. The scale
// has five positions and therefore FOUR steps, and its bottom position
// is 1, not 0 — dividing by the maximum would silently assert that the
// lowest confidence a learner can express means "20% sure". It doesn't:
// the widget is a bare 1–5 select with no probability anchors
// (web/templates/partials/exercise.html.tmpl), so the only defensible
// reading is that 1 is the bottom of the range and 5 the top. Under the
// wrong mapping a learner who marked every attempt least-confident and
// got nothing right was reported as 20 points OVERconfident, and the
// bottom of the scale could never read as calibrated at all.
const (
	confidenceScaleMin = 1.0
	confidenceScaleMax = 5.0
)

// deriveCalibration fills in MeanConfidence/CorrectRate/Overconfidence
// from each week's raw sums, leaving a week nobody rated at zero rather
// than dividing by it. A zero-Attempts week is NOT a week of zero
// confidence, zero accuracy, or perfect calibration — display code must
// show it as "no data" (see web/templates/outcomes.html.tmpl).
func deriveCalibration(weeks []storage.CalibrationTrend) []storage.CalibrationTrend {
	out := make([]storage.CalibrationTrend, len(weeks))
	copy(out, weeks)
	for i := range out {
		if out[i].Attempts > 0 {
			out[i].MeanConfidence = float64(out[i].ConfidenceTotal) / float64(out[i].Attempts)
			out[i].CorrectRate = float64(out[i].Corrects) / float64(out[i].Attempts)
			out[i].Overconfidence = (out[i].MeanConfidence-confidenceScaleMin)/
				(confidenceScaleMax-confidenceScaleMin) - out[i].CorrectRate
		}
	}
	return out
}

// deriveFading marks each week Measurable and fills in Per1000 for
// those that are, leaving every other week's rate at zero rather than
// dividing by a sample too small to divide by — the same "0 means
// unmeasured, not measured-as-zero" rule as deriveCalibration, with the
// bar raised from "any characters at all" to MinMeasurableRunes (see
// that constant for why "any" was not enough). Runes and Corrections
// are untouched: they are raw facts and stay displayable either way.
func deriveFading(weeks []storage.WeeklyRate) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, len(weeks))
	copy(out, weeks)
	for i := range out {
		out[i].Measurable = out[i].Runes >= MinMeasurableRunes
		if out[i].Measurable {
			out[i].Per1000 = float64(out[i].Corrections) / float64(out[i].Runes) * 1000
		}
	}
	return out
}

// recentReviewedRunes totals the characters submitted for review inside
// the recent window — the window CorrectionsAfter is counted over, so
// the two describe the same stretch of time.
//
// A week bucket counts when it OVERLAPS the window at all, not only
// when it starts inside it: the window boundary falls mid-week roughly
// six times in seven, and dropping the straddling week would discard up
// to six days of genuine writing and report an active learner as
// inactive. Erring toward "the learner was writing" is the right
// direction for a threshold whose only power is to WITHHOLD a
// judgement — it means the guard fires when there is real doubt, not
// merely when the calendar is awkward.
func recentReviewedRunes(weeks []storage.WeeklyRate, now time.Time) int {
	cutoff := now.Add(-storage.OutcomeRecentWindow)
	total := 0
	for _, w := range weeks {
		if w.WeekStart.Add(weekSpan).After(cutoff) {
			total += w.Runes
		}
	}
	return total
}

// notEnoughData is what a learner with no measurable history sees. It
// is the single most important string in this package: it is what every
// new user reads, and the alternatives ("0% improvement", an empty
// chart, a cheerful placeholder) would each claim something untrue.
const notEnoughData = "Not enough data yet to say whether corrections are improving later production."

// headline builds the page's one sentence from whatever facts exist,
// and only those. Each independent fact is its own clause; with no
// facts at all it returns notEnoughData.
func headline(rep Report) string {
	var clauses []string

	judged := len(rep.Retired) + len(rep.Improving) + len(rep.Persistent)

	// The inactivity caveat leads the sentence whenever there is anything
	// at all to qualify and the learner has barely written lately. It is
	// reachable with concepts judged — the retired rule by design needs
	// no recent activity ("≥3 corrections then 60 silent days"), so
	// without this a learner who walked away would be handed "12
	// concepts retired, 0 improving, 0 still recurring." as though it
	// were a finding about their learning — and equally with everything
	// excluded, where it is the single most useful thing the sentence
	// can say.
	//
	// "counted by whole weeks" is not hedging: RecentRunes is summed
	// from weekly buckets, and the bucket straddling the 30-day boundary
	// counts in full, so the total can reach up to six days further back
	// than a literal "last 30 days" (see recentReviewedRunes for why
	// that direction is the safe one). The sentence says what was
	// actually measured.
	if (judged > 0 || len(rep.Excluded) > 0) && rep.RecentRunes < MinMeasurableRunes {
		clauses = append(clauses, fmt.Sprintf(
			"less than %s characters were submitted for review in the last %d days (counted by whole weeks)",
			thousands(MinMeasurableRunes), int(storage.OutcomeRecentWindow/(24*time.Hour))))
	}

	switch {
	case judged > 0 && len(rep.Improving) == 0 && len(rep.Persistent) == 0:
		// Retired-only. "1 concept retired, 0 improving, 0 still
		// recurring" reads as three measurements, two of which came back
		// empty — but retirement is the one verdict that needs no recent
		// activity at all ("≥3 corrections then 60 silent days"), so the
		// two zeros are almost always an absence of MEASUREMENT, not a
		// measured absence of improvement. That is the same honesty bug
		// the all-excluded branch below already works hard to avoid, on
		// the same page; this branch borrows its fix. The retirement
		// itself is a real finding and is still reported.
		c := countOf(len(rep.Retired), "concept") + " retired; no other concept could be judged yet"
		if n := len(rep.Excluded); n > 0 {
			c += fmt.Sprintf(", %d excluded for insufficient data", n)
		}
		clauses = append(clauses, c)
	case judged > 0:
		c := fmt.Sprintf("%s retired, %d improving, %d still recurring",
			countOf(len(rep.Retired), "concept"), len(rep.Improving), len(rep.Persistent))
		if n := len(rep.Excluded); n > 0 {
			c += fmt.Sprintf(", %d excluded for insufficient data", n)
		}
		clauses = append(clauses, c)
	case len(rep.Excluded) > 0:
		// Every concept was excluded. Reporting "0 improving" here would
		// read as a measured absence of improvement rather than an
		// absence of measurement, so the counts are omitted entirely.
		//
		// The clause deliberately names NO missing ingredient. It used to
		// say "no concept has enough history to judge yet", which is a
		// different false claim in exactly the case the recent-writing
		// guard exists for: a learner who stopped writing has concepts
		// with 200 days of history and was told their history was too
		// short — contradicting the page's own banner, which correctly
		// blamed the missing writing. Exclusions have three possible
		// causes; the per-concept reasons in the table below state the
		// actual one for each, and the caveat clause above states the
		// inactivity case when it applies.
		clauses = append(clauses,
			"no concept could be judged yet",
			fmt.Sprintf("%s excluded for insufficient data", countOf(len(rep.Excluded), "concept")))
	}

	if c := fadingClause(rep.Fading); c != "" {
		clauses = append(clauses, c)
	}
	if len(clauses) == 0 {
		return notEnoughData
	}
	return capitalise(strings.Join(clauses, "; ")) + "."
}

// fadingClause describes how the corrections-per-1000-characters rate
// moved between the FIRST and LAST week carrying a big enough sample to
// have a rate at all — never the first and last week of the window
// (most of which may be empty), and never a week that scraped together
// a handful of characters. It returns "" unless at least two such weeks
// exist: one point is not a trend, and a trend claimed from one point
// is a lie.
//
// The "big enough" test is Measurable, i.e. MinMeasurableRunes — the
// same bar the chart and the table use, so the sentence, the line and
// the numbers can never disagree about which weeks count. Without it a
// five-character first week yielded "fell from 200.0 to 3.4", and a
// 120-character last week yielded "to 0.0" — the very figure the
// zero-data guard forbids, because it reads as "no corrections needed".
//
// The two endpoints are compared as DISPLAYED (one decimal place), so
// the sentence can never read "fell from 3.4 to 3.4" over a difference
// too small to show.
func fadingClause(weeks []storage.WeeklyRate) string {
	first, last := -1, -1
	for i, w := range weeks {
		if !w.Measurable {
			continue
		}
		if first < 0 {
			first = i
		}
		last = i
	}
	if first < 0 || first == last {
		return ""
	}

	from := fmt.Sprintf("%.1f", weeks[first].Per1000)
	to := fmt.Sprintf("%.1f", weeks[last].Per1000)
	span := last - first + 1

	if from == to {
		return fmt.Sprintf("corrections per 1,000 characters held steady at %s over %d weeks", to, span)
	}
	direction := "fell"
	if weeks[last].Per1000 > weeks[first].Per1000 {
		direction = "rose"
	}
	return fmt.Sprintf("corrections per 1,000 characters %s from %s to %s over %d weeks", direction, from, to, span)
}

// countOf renders "1 concept" / "3 concepts" — the noun appears once
// per clause, on the first count, so only this one needs pluralising.
func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// thousands renders n with comma group separators ("1,000"), matching
// the "per 1,000 characters" phrasing already in the headline so both
// figures read as the same kind of number. n is never negative here
// (it is only ever a rune-count threshold).
func thousands(n int) string {
	s := strconv.Itoa(n)
	head := len(s) % 3
	if head == 0 {
		head = 3
	}
	var b strings.Builder
	b.WriteString(s[:head])
	for i := head; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// capitalise upper-cases the first rune only. Clauses are written
// lowercase so any of them can lead the sentence.
func capitalise(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}
