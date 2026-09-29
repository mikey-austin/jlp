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

	// The two Phase 3 Task 3 Anki events (PRD §19): TypeAnkiCardCreated
	// fires once per generated draft (application/anki.Service.
	// GenerateFromCorrection); TypeAnkiCardExported fires once per card
	// actually marked exported, from either ExportTSV or
	// PushToAnkiConnect. There is deliberately no
	// anki.card.approved/rejected event type — SetStatus's transition is
	// visible in the card's own Status column (like a correction's own
	// accept/reject), not duplicated into the event log the way
	// correction.accepted/correction.rejected are, since nothing
	// downstream reacts to an Anki card's review decision the way the
	// learner model reacts to a correction's.
	TypeAnkiCardCreated  Type = "anki.card.created"
	TypeAnkiCardExported Type = "anki.card.exported"

	// TypeVocabularyImported fires once per POST /api/v1/words batch
	// (Phase 3 Task 8, Nihongo Daily's bulk ingestion contract) — never
	// once per word, which would flood the history the same number of
	// rows a batch import touches. Subject is the FIRST word's
	// WordInput.Source when non-empty, or "external" otherwise (not a
	// scan for the first non-empty source across the whole batch);
	// Evidence carries {"count": N, "sample": [...up to 5 imported
	// expressions]}. See application/vocabulary.Service.IngestWords for
	// exactly how Subject/Evidence are built.
	TypeVocabularyImported Type = "vocabulary.imported"

	// The two Phase 3 Task 4 tutor lesson guide events (PRD §18, §60):
	// TypeTutorLessonCreated fires once per generated guide
	// (application/lessons.Service.Generate); TypeTutorLessonCompleted
	// fires once the human tutor's post-lesson observation has been
	// recorded (application/lessons.Service.Complete), Evidence carrying
	// the observation's subjects. The learner model does NOT yet consume
	// either — see Service.Complete's own doc comment — they exist to
	// enrich the activity feed and a future learnermodel rebuild's
	// fidelity.
	TypeTutorLessonCreated   Type = "tutor.lesson.created"
	TypeTutorLessonCompleted Type = "tutor.lesson.completed"

	// The two Phase 4 Task 6 conversation tutor events (PRD §17.4):
	// TypeConversationTurn fires once per application/conversation.
	// Service.Say call, Evidence carrying {"position":…, "corrections":…}
	// — corrections found this turn are NOT separately re-recorded under
	// their own conversation event type; they reuse
	// TypeCorrectionPresented/TypeHintShown, the SAME two event types
	// application/feedback.Service.RequestFeedback records for writing
	// corrections, so a downstream consumer (the learner model,
	// statistics) never has to know whether a correction came from a
	// document review or a conversation turn to react to it.
	// TypeConversationSummarised fires once per Service.Summarise call,
	// Evidence carrying {"turns":…, "corrections":…} — the end-of-
	// conversation digest PRD §17.4's "end" feedback timing releases.
	TypeConversationTurn       Type = "conversation.turn"
	TypeConversationSummarised Type = "conversation.summarised"

	// TypeSpeechTranscribed fires once per successful POST
	// /speech/transcribe call (Phase 4 Task 8, PRD §66): Subject is a
	// freshly generated id (there is no natural row to key it by — the
	// transcript itself is never persisted on its own; it flows
	// straight into whatever the caller does with it next, typically
	// application/conversation.Service.Say over the SAME conversation
	// pipeline typed text already goes through), and Evidence carries
	// {"duration_ms":…, "mime":…, "chars":…} — the clip's own length
	// (from ai.Transcript.DurationMS, NOT how long transcription took),
	// the uploaded audio's declared Content-Type, and the transcript's
	// rune length. This is deliberately a session-less event
	// (SessionID nil) like TypeVocabularyImported: recording speech is
	// scoped to the identity that spoke, not to whichever session's
	// conversation pane happened to be open.
	TypeSpeechTranscribed Type = "speech.transcribed"

	// The two Phase 4 Task D soft-delete events: TypeContentDeleted
	// fires when a learner hides a session, a vocabulary item or a
	// lesson; TypeContentRestored fires when they bring one back
	// (/sessions' undo affordance, or `jlp restore`). Subject is the
	// row's id and Evidence carries {"kind": "session" | "vocabulary" |
	// "lesson"} — the kind cannot be inferred from Subject, since all
	// three are bare UUIDs.
	//
	// Recording a deletion as an event is not a contradiction of soft
	// delete, it is the point of it: learning_events is immutable and
	// append-only, so hiding content leaves no trace anywhere else, and
	// without these two types the log would show a learner's practice
	// but never show them tidying up afterwards. Nothing consumes them
	// yet — the learner model deliberately does not react, exactly like
	// TypeAnkiCardCreated — they exist so the audit trail of what was
	// removed, and when, survives.
	//
	// Session-less (SessionID nil) even when the deleted thing IS a
	// session, matching TypeVocabularyImported and
	// TypeSpeechTranscribed: attaching a deletion to the very session it
	// deletes would file the record inside the thing it is a record of.
	TypeContentDeleted  Type = "content.deleted"
	TypeContentRestored Type = "content.restored"

	// The two 読解 (reading) pipeline events. TypeReadingEditionCreated
	// fires once per study edition whose analysis succeeded
	// (application/reading's worker), Subject the edition id, Evidence
	// carrying the article id and how many vocabulary/grammar entries
	// the lesson has. TypeReadingEditionDelivered fires once per
	// successful Send-to-Kindle delivery. Session-less (SessionID nil),
	// like TypeVocabularyImported: reading an article happens outside any
	// writing session. Neither is consumed by the learner model yet —
	// they are recorded now so the reading history exists when a later
	// task teaches the model what "read an article about X" means; the
	// vocabulary a learner chooses to keep from an edition already flows
	// in through the ordinary vocabulary.looked-up path.
	TypeReadingEditionCreated   Type = "reading.edition.created"
	TypeReadingEditionDelivered Type = "reading.edition.delivered"
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
