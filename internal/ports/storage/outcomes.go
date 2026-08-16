package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// OutcomeBaselineWindow and OutcomeRecentWindow are the two measurement
// windows every ConceptOutcome's CorrectionsBefore/CorrectionsAfter are
// counted over. They live here, on the port, rather than inside either
// the SQL adapter or application/outcomes because BOTH sides need the
// same numbers and they must never drift apart:
//
//   - the adapter passes them into db/queries/outcomes.sql as query
//     parameters (the SQL itself hard-codes no window),
//   - application/outcomes adds them together to know how much history
//     a concept needs before the two windows stop OVERLAPPING, which is
//     the single most important honesty rule on the /outcomes page (see
//     that package's minimumHistory).
//
// These are the definition of what the two counts MEAN, so they belong
// to the data contract. The classification thresholds derived from them
// — how many baseline corrections are enough, how long a silence
// retires a concept — deliberately do NOT live here: those are
// judgements, and judgements live in application/outcomes (project
// convention: SQL returns raw counts, the application layer derives
// judgements).
const (
	// OutcomeBaselineWindow is how long after a concept's FIRST
	// correction the "before" count is accumulated over — the period
	// during which the learner had only just been told, so corrections
	// in it measure the problem, not the remedy.
	OutcomeBaselineWindow = 30 * 24 * time.Hour
	// OutcomeRecentWindow is how far back from now the "after" count is
	// accumulated over — what the learner's production looks like today.
	OutcomeRecentWindow = 30 * 24 * time.Hour
)

// ConceptOutcome is the raw evidence for one grammar concept the
// learner has actually been corrected on: did being corrected on it
// lead to being corrected on it LESS later (PRD §52/§72/§73)?
//
// Every field here is a raw count or timestamp read straight out of
// Postgres. Nothing on this struct is a judgement — in particular
// Retired is left false by the repository and is filled in by
// application/outcomes.Analyser from TotalCorrections/LastCorrectedAt,
// the same raw-counts-in-SQL / derivation-in-Go split Statistics'
// AcceptanceRate and ConfidenceCalibration's CorrectRate already use
// (see analytics.go). A repository that decided "retired" itself would
// bury a 60-day threshold in SQL where no unit test can reach it.
type ConceptOutcome struct {
	Slug string
	Name string
	// FirstCorrectedAt is the earliest correction on this concept for
	// this identity. Everything else is measured relative to it.
	FirstCorrectedAt time.Time
	// LastCorrectedAt is the most recent correction on this concept.
	// Raw input to Analyser's retired judgement — never a judgement
	// itself.
	LastCorrectedAt time.Time
	// TotalCorrections is every correction on this concept, ever, for
	// this identity. Raw input to the retired judgement's "after ≥3
	// prior" half.
	TotalCorrections int
	// CorrectionsBefore counts corrections on this concept inside
	// [FirstCorrectedAt, FirstCorrectedAt+OutcomeBaselineWindow) — the
	// baseline. It is always ≥ 1 (the first correction is inside its
	// own window by construction).
	CorrectionsBefore int
	// CorrectionsAfter counts corrections on this concept inside
	// [now-OutcomeRecentWindow, now] — today's rate. Comparing it with
	// CorrectionsBefore is only meaningful once the two windows have
	// stopped overlapping; enforcing that is Analyser's job.
	CorrectionsAfter int
	// IndependentSolves counts correction.retried learning events for
	// this identity that carried independent=true (recalled without
	// ever revealing the answer — see application/feedback.Service.
	// RetryCorrection) and listed this concept in their Evidence
	// "concepts" array.
	IndependentSolves int
	// ProducedCorrectly counts distinct vocabulary.produced-correctly
	// learning events, after FirstCorrectedAt, for expressions linked
	// to this concept. "Linked" is the only link the schema offers: the
	// expression occurs inside the REPLACEMENT text of one of this
	// identity's corrections tagged with this concept — i.e. the
	// corrected form contained that word, and the learner later used
	// that word correctly of their own accord. See
	// db/queries/outcomes.sql's ConceptProducedCorrectly for the exact
	// join and its cost.
	ProducedCorrectly int
	// Retired is a JUDGEMENT, left false by the repository and filled
	// in by application/outcomes.Analyser: ≥3 corrections in total and
	// none for 60 days.
	Retired bool
}

// CalibrationTrend is one ISO week of the learner's confidence
// calibration — does what they THINK they know match what they can
// actually do, and is that gap closing? Zero-filled: a week with no
// confidence-rated attempts is an explicit entry with Attempts == 0,
// never a gap, so callers zipping this against a fixed x-axis never
// special-case a hole. A zero-Attempts week has no meaningful mean
// confidence or correct rate — display code must render it as "no
// data", NOT as 0% (that would claim a total failure that never
// happened).
//
// The repository returns Attempts/ConfidenceTotal/Corrects and leaves
// MeanConfidence/CorrectRate zero; application/outcomes derives both.
type CalibrationTrend struct {
	WeekStart time.Time
	// Attempts is confidence-rated exercise attempts that week.
	Attempts int
	// ConfidenceTotal is the sum of those attempts' self-reported
	// confidence values (each 1..5).
	ConfidenceTotal int
	// Corrects is how many of those attempts were correct.
	Corrects int
	// MeanConfidence is ConfidenceTotal/Attempts (0 when Attempts is 0).
	MeanConfidence float64
	// CorrectRate is Corrects/Attempts (0 when Attempts is 0).
	CorrectRate float64
	// Overconfidence is the calibration error itself: the mean
	// confidence expressed on the same 0..1 scale as CorrectRate, minus
	// CorrectRate. Positive means the learner was surer than they were
	// right; negative means they knew more than they thought. 0 when
	// Attempts is 0 — which is "not measured", not "perfectly
	// calibrated". Derived by application/outcomes.
	Overconfidence float64
}

// WeeklyRate is one ISO week of assistance fading: how much text the
// learner submitted for review, and how many corrections that text
// needed. Zero-filled like CalibrationTrend — a week with no reviews at
// all is Runes == 0, which is NOT "a week that needed no help", it is a
// week with nothing to measure; Per1000 stays 0 and display code must
// distinguish the two.
//
// Runes counts characters SUBMITTED FOR FEEDBACK (feedback_requests.
// selection_text), not every character ever typed — the denominator
// that makes the ratio answer "how much help did the same amount of
// reviewed writing need?". That deliberately differs from the home
// dashboard's lifetime Statistics.CorrectionsPer1000, whose denominator
// is every document's full content; the two figures are not expected to
// match and the /outcomes page does not present them side by side.
type WeeklyRate struct {
	WeekStart time.Time
	// Runes is characters submitted for review that week.
	Runes int
	// Corrections is how many corrections those submissions produced.
	Corrections int
	// Per1000 is Corrections/Runes*1000 (0 when Runes is 0), derived by
	// application/outcomes — never by SQL.
	Per1000 float64
}

// OutcomeRepository reads the raw learning-outcome evidence behind the
// /outcomes page. Every method is identity-scoped: another identity's
// corrections, attempts, and reviews are invisible here, exactly as in
// every other repository in this package.
type OutcomeRepository interface {
	// ConceptOutcomes returns one row per grammar concept this identity
	// has ever been corrected on, ordered by slug. Concepts the
	// identity has never been corrected on are absent entirely (there
	// is no outcome to report), as are corrections tagged with an
	// unresolved slug. Retired is always false — see its doc comment.
	ConceptOutcomes(ctx context.Context, identity learner.IdentityID) ([]ConceptOutcome, error)
	// CalibrationTrend returns exactly weeks entries, oldest first,
	// zero-filled, ending with the ISO week containing now.
	// MeanConfidence/CorrectRate are always zero — the caller derives
	// them.
	CalibrationTrend(ctx context.Context, identity learner.IdentityID, weeks int) ([]CalibrationTrend, error)
	// AssistanceFading returns exactly weeks entries, oldest first,
	// zero-filled, ending with the ISO week containing now. Per1000 is
	// always zero — the caller derives it.
	AssistanceFading(ctx context.Context, identity learner.IdentityID, weeks int) ([]WeeklyRate, error)
}
