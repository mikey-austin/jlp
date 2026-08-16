package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/correction"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// ConversationTurn is one exchange in the conversation tutor (Phase 4
// Task 6, PRD §17.4): the learner's message, the tutor's reply, and
// whatever corrections the conversation agent found — stored whole
// (jsonb), not split into a separate child table the way
// FeedbackRecord/CorrectionRecord are, since a conversation turn's
// corrections have no independent lifecycle of their own (no per-
// correction accept/reject/retry/reveal flow — see
// application/conversation.Service's doc comment for why). Position is
// 1-indexed and strictly increasing within a conversation, the ordering
// application/conversation.Service's feedback-timing logic (immediate/
// delayed/end) counts turns by.
type ConversationTurn struct {
	ID             string
	ConversationID string
	SessionID      session.ID
	Position       int
	LearnerText    string
	Reply          string
	ReplyEN        string
	Followup       string
	Corrections    []correction.Correction
	AIRequestID    string
	CreatedAt      time.Time
}

// ConversationRepository persists the conversation tutor's transcript.
// Every method is identity-scoped: GetOrCreateForSession and
// ListTurns both take the caller's identity, and InsertTurn's
// conversationID must belong to that identity (checked via a join to
// conversations, the same "never trust the caller, check via a join"
// pattern db/queries/lessons.sql's InsertLessonObservation uses) — a
// wrong identity or unknown ID misses with ErrNotFound, never another
// identity's transcript.
type ConversationRepository interface {
	// GetOrCreateForSession returns the session's single conversation
	// (one conversation per session, like DocumentRepository's one
	// document per session), creating it on first use. The bool return
	// is true only when this call created it.
	GetOrCreateForSession(ctx context.Context, identity learner.IdentityID, sid session.ID) (conversationID string, created bool, err error)
	// InsertTurn appends turn to conversationID, identity-scoped via a
	// join to conversations: a conversationID that doesn't belong to
	// identity fails with ErrNotFound and writes nothing.
	InsertTurn(ctx context.Context, identity learner.IdentityID, conversationID string, turn ConversationTurn) error
	// ListTurns returns every turn in conversationID, oldest first,
	// identity-scoped the same way InsertTurn is.
	ListTurns(ctx context.Context, identity learner.IdentityID, conversationID string) ([]ConversationTurn, error)
}
