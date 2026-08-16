package event

// AllTypes returns every declared learning-event Type, in the same
// order they're declared in event.go. It exists for consumers that
// must react to every event type there is, regardless of what it is
// — currently only internal/adapters/mqtt.Bridge's outbound bridge,
// which has no other way to enumerate ports/events.EventBus's
// per-type Subscribe (that interface has no "subscribe to
// everything" method by design — see its own doc comment).
//
// This list is curated by hand, not derived via reflection, so
// alltypes_test.go's TestAllTypesMatchesDeclaredConstants parses
// event.go's own const declarations and fails the build the moment a
// new Type constant is added here without a matching AllTypes()
// entry (or vice versa) — see that test for the drift-detection
// mechanics.
func AllTypes() []Type {
	return []Type{
		TypeWritingCreated,
		TypeWritingUpdated,
		TypeFeedbackRequested,
		TypeCorrectionPresented,
		TypeCorrectionAccepted,
		TypeCorrectionRejected,
		TypeGrammarConceptEncountered,
		TypeVocabularyLookedUp,
		TypeVocabularyProduced,
		TypeVocabularyProducedCorrectly,
		TypeHintShown,
		TypeCorrectionRetried,
		TypeAnswerRevealed,
		TypeConfidenceRecorded,
		TypeQuizStarted,
		TypeQuizAnswered,
		TypeQuizCompleted,
		TypeAnkiCardCreated,
		TypeAnkiCardExported,
		TypeVocabularyImported,
		TypeTutorLessonCreated,
		TypeTutorLessonCompleted,
		TypeConversationTurn,
		TypeConversationSummarised,
	}
}
