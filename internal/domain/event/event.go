package event

import (
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// Type identifies a kind of learning event. Every later capability (AI
// feedback, corrections, statistics) is a producer or consumer of these.
type Type string

const (
	TypeWritingCreated      Type = "writing.created"
	TypeWritingUpdated      Type = "writing.updated"
	TypeFeedbackRequested   Type = "feedback.requested"
	TypeCorrectionPresented Type = "correction.presented"
	TypeCorrectionAccepted  Type = "correction.accepted"
	TypeCorrectionRejected  Type = "correction.rejected"
)

// LearningEvent is an immutable, append-only record of something a learner
// did or experienced. Rows are never updated or deleted once written.
type LearningEvent struct {
	ID         string
	IdentityID learner.IdentityID
	SessionID  *session.ID // nil for session-less events
	Type       Type
	Subject    string         // e.g. document ID, correction ID
	Evidence   map[string]any // stored as jsonb
	OccurredAt time.Time
}
