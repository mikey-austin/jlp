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
// FeedbackRepository.UpdateCorrectionStatus.
type CorrectionRecord struct {
	ID, FeedbackID               string
	Position                     int
	Original, Replacement        string
	Type, Severity               string
	ExplanationJA, ExplanationEN string
	Status                       string // "presented" | "accepted" | "rejected"
}

// FeedbackRepository persists AI feedback requests and their
// corrections. InsertFeedback writes a FeedbackRecord and its
// corrections atomically (one tx): a review the learner sees always
// has a durable trace, never one without the other.
// UpdateCorrectionStatus re-checks identity via a join to
// feedback_requests, mirroring the other repositories' identity-scoped
// access control, so one learner can never mutate another's
// correction.
type FeedbackRepository interface {
	InsertFeedback(ctx context.Context, rec FeedbackRecord, corrections []CorrectionRecord) error
	UpdateCorrectionStatus(ctx context.Context, identity learner.IdentityID, correctionID, status string) (CorrectionRecord, error)
}
