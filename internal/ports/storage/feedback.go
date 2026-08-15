package storage

import (
	"context"
	"time"

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
}
