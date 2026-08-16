package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/domain/writing"
)

// FeedbackRecord is the persisted shape of one AI review of a document
// selection: what was reviewed, when, and the corrected text the model
// produced. CorrectionRecord rows (below) carry the individual
// suggestions; a FeedbackRecord's own status never changes once
// written — only its corrections do.
type FeedbackRecord struct {
	ID             string
	IdentityID     learner.IdentityID
	SessionID      session.ID
	DocumentID     writing.DocumentID
	SelectionStart int // rune offset
	SelectionEnd   int // rune offset
	SelectionText  string
	CorrectedText  string
	AIRequestID    string
	CreatedAt      time.Time
}

// CorrectionRecord is one correction offered as part of a
// FeedbackRecord. Status starts "presented" and transitions to
// "accepted"/"rejected" as the learner responds — see
// FeedbackRepository.UpdateCorrectionStatus (a plain accept/reject),
// RetryCorrection (accepted automatically by a correct retry — see that
// method's doc comment), and RevealCorrection (leaves Status alone,
// only flips Revealed). SessionID is only populated by the
// identity-scoped read/write methods below (via a join back to the
// owning feedback_requests row): InsertFeedback's caller already has it
// on the FeedbackRecord, so it isn't duplicated onto the
// CorrectionRecord there.
//
// HintJA/HintEN, Attempts, Confidence, and Revealed are Phase 2 Task
// 8's active-recall/confidence-tracking columns (PRD §9/§53):
// HintJA/HintEN mirror correction.Explanation but are always both-empty
// for a non-socratic correction (InsertFeedback writes "" for both, the
// column defaults match); Attempts only ever advances via
// RetryCorrection; Confidence is nil until RecordConfidence sets it
// (1..5, DB-enforced via a CHECK constraint — see migration 00012);
// Revealed only ever flips true, via RevealCorrection.
type CorrectionRecord struct {
	ID, FeedbackID               string
	Position                     int
	Original, Replacement        string
	Type, Severity               string
	ExplanationJA, ExplanationEN string
	HintJA, HintEN               string
	Status                       string // "presented" | "accepted" | "rejected"
	Attempts                     int
	Confidence                   *int // nil until RecordConfidence sets it (1..5)
	Revealed                     bool
	SessionID                    session.ID
}

// HasHint reports whether c carries a socratic hint — the
// storage.CorrectionRecord equivalent of correction.Correction.HasHint,
// which CorrectionRecord can't call directly since it doesn't embed
// correction.Correction (it flattens Hint into HintJA/HintEN alongside
// every other field, matching every other *_record shape's flat, DB-row
// layout).
func (c CorrectionRecord) HasHint() bool {
	return c.HintJA != "" || c.HintEN != ""
}

// IsGated reports whether c is still withheld from the learner under
// Phase 2's socratic active-recall gate (PRD §9/§53) — see
// correction.IsGated, the single predicate this delegates to so it can
// never drift from the HTML correction_card partial's or the JSON
// API's own gate check. application/anki.Service.GenerateFromCorrection
// and application/lessons.Service.Generate both call this directly on
// the CorrectionRecord(s) storage.FeedbackRepository hands them.
func (c CorrectionRecord) IsGated() bool {
	return correction.IsGated(c.HasHint(), c.Status, c.Revealed)
}

// FeedbackSummary is one row of a session's feedback history (Phase 4
// Task W item 2) — the workspace's right-hand history list, which
// replaces the old activity feed: enough to render one row (a
// timestamp, an excerpt of what was reviewed, how many corrections
// came back, and which provider/model served it) without pulling every
// correction's full detail. Provider/Model are "" when AIRequestID was
// never set on the underlying feedback_requests row (a fakeai-backed
// review outside the observability decorator — see
// FeedbackRecord.AIRequestID's doc comment), exactly like
// FeedbackDetail below.
type FeedbackSummary struct {
	ID              string
	SelectionText   string
	CorrectionCount int
	Provider        string
	Model           string
	CreatedAt       time.Time
}

// FeedbackDetail is one feedback_requests row plus the ai_requests
// provenance the workspace's history detail view needs (Phase 4 Task W
// items 2/3, GetFeedback below): clicking an older history row must
// show the SAME "which provider/model actually served it" the fresh
// result does (feedback.Service.RequestFeedback reads that straight off
// ai.StructuredResponse; a historical read has no response in memory,
// so it comes from the ai_requests row instead, joined by
// FeedbackRecord.AIRequestID). Provider/Model are "" under the same
// "never observed" condition as FeedbackSummary above.
type FeedbackDetail struct {
	ID            string
	SelectionText string
	CorrectedText string
	Provider      string
	Model         string
	CreatedAt     time.Time
}

// ConceptTag is one grammar-concept tag a correction carries (Phase 2
// Task 2). Resolved reports whether Slug was found in the
// GrammarRepository catalog at tag-time — an unknown slug is still
// persisted (Resolved=false), never dropped, so a tagging bug or a
// stale candidate list surfaces as data rather than silently
// vanishing. The caller (application/feedback.Service) is responsible
// for deduplicating a correction's tagged slugs before building
// ConceptTags — InsertFeedback writes exactly what it's given, one row
// per entry.
type ConceptTag struct {
	Slug     string
	Resolved bool
}

// FeedbackRepository persists AI feedback requests and their
// corrections. InsertFeedback writes a FeedbackRecord, its
// corrections, AND their concept tags atomically — all in ONE
// transaction, never split across separate calls: a review the
// learner sees always has a durable trace, and a failure partway
// through (say, a concept row hitting a constraint) must roll back the
// feedback_requests/corrections rows too, not leave them committed
// while the client sees an error and retries under fresh IDs (which
// would otherwise double-count concept encounters downstream). concepts
// is keyed by correction ID — corrections[i].ID — with one []ConceptTag
// per correction that has any; a correction absent from the map (or
// present with an empty slice) simply gets no correction_concepts rows.
// UpdateCorrectionStatus re-checks identity via a join to
// feedback_requests, mirroring the other repositories' identity-scoped
// access control, so one learner can never mutate another's
// correction. GetCorrectionConcepts reads concept tags back — RESOLVED
// slugs only, in a deterministic (slug-ascending) order — so a caller
// re-rendering a correction after its status changes
// (SetCorrectionStatus) can carry its concept chip(s) along; an
// unresolved tag is deliberately excluded here, matching the same
// "only resolved counts" semantics db/queries/grammar.sql's
// ConceptStats/CorrectionsForConcept use.
//
// RetryCorrection, RevealCorrection, and RecordConfidence (Phase 2 Task
// 8) share UpdateCorrectionStatus's identity-scoped join pattern —
// wrong identity or unknown correction ID both come back ErrNotFound.
// RetryCorrection and RevealCorrection additionally restrict the match
// to corrections still Status "presented" (the only state active
// recall applies to — see correction_card.html.tmpl's gate), so an
// attempt against an already-resolved correction ALSO comes back
// ErrNotFound, same as a wrong identity: callers can't and don't need
// to distinguish "not yours" from "not found" from "not retriable"
// here, matching this package's existing identity-scoped convention.
// trimmedAttempt is compared to the stored Replacement by the
// implementation (exact string equality — byte-exact is rune-exact for
// valid UTF-8 — see application/feedback.Service.RetryCorrection's doc
// comment for why the trim happens in the service, not here); the
// returned CorrectionRecord's Status is "accepted" when it matched,
// unchanged otherwise, and Attempts always reflects the NEW
// (post-increment) count. RecordConfidence does NOT restrict by
// Status — a learner may rate their confidence any time after a
// correction resolves, and repeated calls simply overwrite the value;
// confidence itself is validated by the caller (see
// application/feedback.Service.RecordConfidence) before this method is
// ever reached, since the DB's CHECK constraint alone would surface as
// an opaque constraint-violation error rather than a clean, testable
// application error.
type FeedbackRepository interface {
	InsertFeedback(ctx context.Context, rec FeedbackRecord, corrections []CorrectionRecord, concepts map[string][]ConceptTag) error
	UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionRecord, error)
	GetCorrectionConcepts(ctx context.Context, correctionID string) ([]string, error)
	RetryCorrection(ctx context.Context, identity learner.IdentityID, correctionID, trimmedAttempt string) (CorrectionRecord, error)
	RevealCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (CorrectionRecord, error)
	RecordConfidence(ctx context.Context, identity learner.IdentityID, correctionID string, confidence int) (CorrectionRecord, error)
	// GetCorrection reads back one correction (Phase 3 Task 3, PRD §19):
	// application/anki.Service.GenerateFromCorrection needs the
	// correction's Original/Replacement/Explanation to give the Anki
	// agent something to write a card from. Identity-scoped via the same
	// join to feedback_requests UpdateCorrectionStatus uses — a wrong
	// identity or unknown correction ID both miss with ErrNotFound.
	GetCorrection(ctx context.Context, identity learner.IdentityID, correctionID string) (CorrectionRecord, error)
	// RecentCorrections reads back identity's most recent corrections
	// across every session, newest first, at most limit (Phase 3 Task 4,
	// PRD §18): application/lessons.Service.Generate uses this to give
	// the lesson agent recent, concrete examples of the learner's own
	// mistakes to reference — the same identity-scoped join
	// GetCorrection uses (via feedback_requests), but unfiltered by
	// status: a lesson guide benefits from seeing what was corrected
	// regardless of whether the learner has since accepted, rejected, or
	// not yet responded to it.
	RecentCorrections(ctx context.Context, identity learner.IdentityID, limit int) ([]CorrectionRecord, error)
	// ListForSession returns identity's feedback history for sessionID,
	// most-recent-first (Phase 4 Task W item 2) — identity-scoped like
	// every other read here, via the same identity_id column every other
	// method in this file filters by. A sessionID that exists but
	// belongs to another identity returns an EMPTY slice, not
	// ErrNotFound: this is a list, not a single-resource lookup, and
	// "no rows matched this identity" is indistinguishable from "no
	// feedback yet" — exactly RecentCorrections' own empty-vs-error
	// convention for the same reason.
	ListForSession(ctx context.Context, identity learner.IdentityID, sessionID session.ID) ([]FeedbackSummary, error)
	// GetFeedback returns feedbackID's own FeedbackDetail and every one
	// of its corrections, ordered by Position (Phase 4 Task W items
	// 2/3): the workspace history list's "click a row, load it into the
	// bottom pane" GET. Identity-scoped via the same feedback_requests
	// join UpdateCorrectionStatus/GetCorrection use — a feedbackID that
	// exists but belongs to another identity misses with ErrNotFound,
	// exactly like an unknown one. The returned CorrectionRecords are
	// NOT separately identity-checked (their feedback_request_id is
	// already proven to belong to identity by this same call), matching
	// InsertFeedback's own "the parent row is the authorization
	// boundary" precedent — callers must not skip straight to a
	// correction-level query with an unverified feedbackID.
	GetFeedback(ctx context.Context, identity learner.IdentityID, feedbackID string) (FeedbackDetail, []CorrectionRecord, error)
}
