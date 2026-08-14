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
// FeedbackRepository.UpdateCorrectionStatus. SessionID is only
// populated by UpdateCorrectionStatus (via a join back to the owning
// feedback_requests row): InsertFeedback's caller already has it on
// the FeedbackRecord, so it isn't duplicated onto the CorrectionRecord
// there.
type CorrectionRecord struct {
	ID, FeedbackID               string
	Position                     int
	Original, Replacement        string
	Type, Severity               string
	ExplanationJA, ExplanationEN string
	Status                       string // "presented" | "accepted" | "rejected"
	SessionID                    session.ID
}

// FeedbackRepository persists AI feedback requests and their
// corrections. InsertFeedback writes a FeedbackRecord and its
// corrections atomically (one tx): a review the learner sees always
// has a durable trace, never one without the other.
// UpdateCorrectionStatus re-checks identity via a join to
// feedback_requests, mirroring the other repositories' identity-scoped
// access control, so one learner can never mutate another's
// correction. InsertCorrectionConcepts persists a correction's
// grammar-concept tags (Phase 2 Task 2): slugs is every concept the
// teacher agent tagged the correction with, resolved reports per-slug
// whether it was found in the GrammarRepository catalog at tag-time —
// an unknown slug is still recorded (resolved=false), never dropped,
// so a tagging bug or a stale candidate list surfaces as data rather
// than silently vanishing. Idempotent: re-inserting the same
// (correctionID, slug) pair is a no-op (ON CONFLICT DO NOTHING).
type FeedbackRepository interface {
	InsertFeedback(ctx context.Context, rec FeedbackRecord, corrections []CorrectionRecord) error
	UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionRecord, error)
	InsertCorrectionConcepts(ctx context.Context, correctionID string, slugs []string, resolved map[string]bool) error
}
