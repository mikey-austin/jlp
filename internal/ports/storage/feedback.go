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
type FeedbackRepository interface {
	InsertFeedback(ctx context.Context, rec FeedbackRecord, corrections []CorrectionRecord, concepts map[string][]ConceptTag) error
	UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionRecord, error)
	GetCorrectionConcepts(ctx context.Context, correctionID string) ([]string, error)
}
