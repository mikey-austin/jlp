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
//  1. Silence is not evidence. A concept whose two measurement windows
//     still overlap, or whose baseline holds one or two corrections, is
//     EXCLUDED — not "improving", not "persistent". The Report carries
//     those exclusions with a per-concept reason so the page can say how
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
	for _, c := range concepts {
		// Copied by value before anything is written to it: the
		// repository's slice is the caller's, and Retired below is a
		// judgement this package owns, not data the repository handed
		// over.
		outcome := c
		switch group, reason := classify(outcome, now); group {
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
//  2. Then the two exclusions, because a concept that cannot be
//     measured must never fall through into a judged group.
//  3. Only then the before/after comparison.
//
// The returned reason is non-empty only for groupExcluded.
func classify(c storage.ConceptOutcome, now time.Time) (group, string) {
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
		return groupImproving, ""
	}
	return groupPersistent, ""
}

// confidenceScaleMax is the top of the self-reported confidence scale,
// fixed by the exercise_attempts_confidence_range CHECK constraint
// (1..5) in the 00013 migration. It exists only to put MeanConfidence
// on the same 0..1 scale as CorrectRate so the two can be subtracted.
const confidenceScaleMax = 5.0

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
			out[i].Overconfidence = out[i].MeanConfidence/confidenceScaleMax - out[i].CorrectRate
		}
	}
	return out
}

// deriveFading fills in Per1000, leaving a week with nothing submitted
// for review at zero rather than dividing by it — same "0 means
// unmeasured, not measured-as-zero" rule as deriveCalibration.
func deriveFading(weeks []storage.WeeklyRate) []storage.WeeklyRate {
	out := make([]storage.WeeklyRate, len(weeks))
	copy(out, weeks)
	for i := range out {
		if out[i].Runes > 0 {
			out[i].Per1000 = float64(out[i].Corrections) / float64(out[i].Runes) * 1000
		}
	}
	return out
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
	switch {
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
		clauses = append(clauses,
			"no concept has enough history to judge yet",
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
// moved between the FIRST and LAST week that actually had text to
// measure — never the first and last week of the window, most of which
// may be empty. It returns "" unless at least two such weeks exist: one
// point is not a trend, and a trend claimed from one point is a lie.
//
// The two endpoints are compared as DISPLAYED (one decimal place), so
// the sentence can never read "fell from 3.4 to 3.4" over a difference
// too small to show.
func fadingClause(weeks []storage.WeeklyRate) string {
	first, last := -1, -1
	for i, w := range weeks {
		if w.Runes == 0 {
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
