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
	TypeWritingCreated            Type = "writing.created"
	TypeWritingUpdated            Type = "writing.updated"
	TypeFeedbackRequested         Type = "feedback.requested"
	TypeCorrectionPresented       Type = "correction.presented"
	TypeCorrectionAccepted        Type = "correction.accepted"
	TypeCorrectionRejected        Type = "correction.rejected"
	TypeGrammarConceptEncountered Type = "grammar.concept.encountered"

	// The three vocabulary events (Phase 2 Task 6, PRD §12): looked-up
	// fires on every application/vocabulary.Service.Ingest call;
	// produced/produced-correctly fire from DetectProduction when a
	// looked-up expression turns up in the learner's own writing —
	// produced-correctly when no correction touched its occurrence,
	// produced otherwise. See DetectProduction's doc comment for the
	// exact "touched" definition.
	TypeVocabularyLookedUp          Type = "vocabulary.looked-up"
	TypeVocabularyProduced          Type = "vocabulary.produced"
	TypeVocabularyProducedCorrectly Type = "vocabulary.produced-correctly"

	// The four Phase 2 Task 8 active-recall/confidence-tracking events
	// (PRD §9/§53): TypeHintShown fires once per socratic correction,
	// alongside its correction.presented (application/feedback.Service.
	// RequestFeedback); TypeCorrectionRetried fires on every
	// POST /corrections/{id}/retry, whether the attempt was right or
	// wrong (Evidence carries which); TypeAnswerRevealed fires on
	// POST /corrections/{id}/reveal; TypeConfidenceRecorded fires on
	// POST /corrections/{id}/confidence. See
	// application/feedback.Service's RetryCorrection/RevealCorrection/
	// RecordConfidence for exactly what each Evidence map carries.
	TypeHintShown          Type = "hint.shown"
	TypeCorrectionRetried  Type = "correction.retried"
	TypeAnswerRevealed     Type = "answer.revealed"
	TypeConfidenceRecorded Type = "confidence.recorded"

	// The three Phase 2 Task 9 drill-engine events (PRD §17.2/§58):
	// TypeQuizStarted fires once per generated exercise
	// (application/practice.Service.Start), Evidence carrying the
	// exercise's concept/type. TypeQuizAnswered and TypeQuizCompleted
	// both fire together from application/practice.Service.Answer — one
	// practice round has no separate "answered vs completed" phases the
	// way a multi-step correction retry does, so both carry the same
	// Evidence {"concept":…, "type":…, "correct":…, "confidence":…}; they
	// exist as two event types (not one) so a future consumer can react
	// to "an answer happened" independently of "a round finished"
	// without the two meanings being conflated under one type the way
	// TypeCorrectionRetried's own Evidence.correct already conflates
	// attempt-vs-outcome for corrections.
	TypeQuizStarted   Type = "quiz.started"
	TypeQuizAnswered  Type = "quiz.answered"
	TypeQuizCompleted Type = "quiz.completed"
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
