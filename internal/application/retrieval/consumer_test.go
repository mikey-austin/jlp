package retrieval_test

import (
	"context"
	"errors"
	"testing"
	"time"

	apperetrieval "github.com/mikeyaustin/jlp/internal/application/retrieval"
	"github.com/mikeyaustin/jlp/internal/domain/event"
)

func newConsumerHarness() (*apperetrieval.Consumer, *fakeRetrievalRepo) {
	retrievalRepo := newFakeRetrievalRepo()
	sched := apperetrieval.NewScheduler(retrievalRepo, fixedClock(time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC)))
	consumer := apperetrieval.NewConsumer(sched)
	return consumer, retrievalRepo
}

// TestHandleQuizAnsweredSchedulesConcept pins quiz.answered's wiring:
// Evidence {"concept":…, "correct":…, "confidence":…} becomes a
// concept-type RecordOutcome.
func TestHandleQuizAnsweredSchedulesConcept(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeQuizAnswered,
		Subject:    "ex-1",
		Evidence:   map[string]any{"concept": "te-form", "type": "fill-in-blank", "correct": true, "confidence": 5},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	got, err := repo.Get(context.Background(), testIdentity, "concept", "te-form")
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	// confidence=5 on a success advances TWO steps from "never
	// scheduled" (-1) -> step 1 (3d) — the interval table's own pinned
	// transition, cross-checked here end to end through the consumer.
	if got.Interval != 3*24*time.Hour {
		t.Errorf("Interval = %v, want 3d (confidence-5 double advance from unscheduled)", got.Interval)
	}
}

// TestHandleQuizAnsweredToleratesFloat64Confidence pins that a replayed
// event (Evidence decoded from jsonb, where JSON numbers become
// float64) is handled identically to a live int — see evidenceInt.
func TestHandleQuizAnsweredToleratesFloat64Confidence(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeQuizAnswered,
		Subject:    "ex-1",
		Evidence:   map[string]any{"concept": "te-form", "correct": true, "confidence": float64(1)},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	got, err := repo.Get(context.Background(), testIdentity, "concept", "te-form")
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	// confidence=1 (shaky) on a success from unscheduled floors at step
	// 0 (1d), matching the interval table.
	if got.Interval != 24*time.Hour {
		t.Errorf("Interval = %v, want 1d (shaky confidence, floored at step 0)", got.Interval)
	}
}

// TestHandleQuizAnsweredMissingConceptIsNoop pins that an Evidence
// without a usable "concept" schedules nothing (never a panic, never a
// spurious zero-subject row).
func TestHandleQuizAnsweredMissingConceptIsNoop(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeQuizAnswered,
		Subject:    "ex-1",
		Evidence:   map[string]any{"correct": true},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if len(repo.items) != 0 {
		t.Fatalf("repo.items = %+v, want none scheduled", repo.items)
	}
}

// TestHandleCorrectionRetriedIndependentSuccessSchedulesConcepts pins
// the brief's "independent solve = success" rule: correct AND never
// revealed schedules a success for every concept slug in
// Evidence["concepts"] — the resolved concept tags
// feedback.Service.RetryCorrection threads through this event (see
// application/retrieval.Consumer's own doc comment on why this
// package never re-queries them itself).
func TestHandleCorrectionRetriedIndependentSuccessSchedulesConcepts(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"attempts": 1, "correct": true, "independent": true, "concepts": []string{"te-form", "particle-wa"}},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	for _, slug := range []string{"te-form", "particle-wa"} {
		got, err := repo.Get(context.Background(), testIdentity, "concept", slug)
		if err != nil {
			t.Fatalf("repo.Get(%q): %v", slug, err)
		}
		if got.Successes != 1 || got.Failures != 0 {
			t.Errorf("%s: Successes/Failures = %d/%d, want 1/0", slug, got.Successes, got.Failures)
		}
	}
}

// TestHandleCorrectionRetriedTOleratesAnySliceConcepts pins
// evidenceStringSlice's []any tolerance: a replayed event whose
// Evidence has been through a jsonb round trip decodes "concepts" as
// []any (interface{} elements), not []string — handled identically.
func TestHandleCorrectionRetriedToleratesAnySliceConcepts(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"correct": true, "independent": true, "concepts": []any{"te-form"}},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if _, err := repo.Get(context.Background(), testIdentity, "concept", "te-form"); err != nil {
		t.Fatalf("repo.Get(te-form): %v", err)
	}
}

// TestHandleCorrectionRetriedRevealedCorrectIsNoop pins that a correct
// retry AFTER the answer was revealed schedules NOTHING — not a
// success (it wasn't independent recall) and not a failure either.
func TestHandleCorrectionRetriedRevealedCorrectIsNoop(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"attempts": 2, "correct": true, "independent": false, "concepts": []string{"te-form"}},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if len(repo.items) != 0 {
		t.Fatalf("repo.items = %+v, want none scheduled (revealed-then-correct is not independent recall)", repo.items)
	}
}

// TestHandleCorrectionRetriedIncorrectSchedulesFailure pins the failure
// path: correct=false schedules a failure for every concept slug,
// regardless of independent.
func TestHandleCorrectionRetriedIncorrectSchedulesFailure(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"attempts": 1, "correct": false, "independent": false, "concepts": []string{"te-form"}},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	got, err := repo.Get(context.Background(), testIdentity, "concept", "te-form")
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if got.Successes != 0 || got.Failures != 1 {
		t.Errorf("Successes/Failures = %d/%d, want 0/1", got.Successes, got.Failures)
	}
}

// TestHandleCorrectionRetriedNoResolvedConceptsIsNoop pins that a
// correction with no concepts in Evidence (an unresolved/hallucinated
// tag, or none at all) schedules nothing and returns no error.
func TestHandleCorrectionRetriedNoResolvedConceptsIsNoop(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"attempts": 1, "correct": true, "independent": true},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if len(repo.items) != 0 {
		t.Fatalf("repo.items = %+v, want none scheduled", repo.items)
	}
}

// TestHandleCorrectionRetriedPropagatesRecordOutcomeError pins that a
// scheduler failure while scheduling one of several concepts is a hard
// error, not silently ignored.
func TestHandleCorrectionRetriedPropagatesRecordOutcomeError(t *testing.T) {
	consumer, repo := newConsumerHarness()
	repo.upsertErr = errors.New("db down")
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionRetried,
		Subject:    "corr-1",
		Evidence:   map[string]any{"correct": true, "independent": true, "concepts": []string{"te-form"}},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err == nil {
		t.Fatal("HandleEvent returned nil, want the propagated RecordOutcome error")
	}
}

// TestHandleVocabularyProducedCorrectlySchedulesExpression pins
// vocabulary.produced-correctly's wiring: ev.Subject IS the expression
// (application/vocabulary.Service.DetectProduction), always a success.
func TestHandleVocabularyProducedCorrectlySchedulesExpression(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeVocabularyProducedCorrectly,
		Subject:    "積もる",
		Evidence:   map[string]any{"item_id": "item-1"},
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	got, err := repo.Get(context.Background(), testIdentity, "expression", "積もる")
	if err != nil {
		t.Fatalf("repo.Get: %v", err)
	}
	if got.Successes != 1 || got.Failures != 0 {
		t.Errorf("Successes/Failures = %d/%d, want 1/0", got.Successes, got.Failures)
	}
}

// TestHandleEventIgnoresUnrelatedTypes pins HandleEvent's defensive
// default: any event type other than the three wired above is a no-op.
func TestHandleEventIgnoresUnrelatedTypes(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ev := event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeCorrectionPresented,
		Subject:    "corr-1",
	}
	if err := consumer.HandleEvent(context.Background(), ev); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if len(repo.items) != 0 {
		t.Fatalf("repo.items = %+v, want none scheduled", repo.items)
	}
}

// A drilled WORD must be scheduled. Reading Evidence["concept"] alone
// made every word drill a no-op here — a word has no ConceptSlug — so a
// word the learner practised was never scheduled, and 復習キュー stayed
// blind to the one page meant for practising it.
func TestHandleQuizAnsweredSchedulesADrilledWord(t *testing.T) {
	consumer, repo := newConsumerHarness()

	err := consumer.HandleEvent(context.Background(), event.LearningEvent{
		IdentityID: testIdentity,
		Type:       event.TypeQuizAnswered,
		Subject:    "ex-1",
		Evidence: map[string]any{
			"subject_type": "word",
			"subject_ref":  "vocab-42",
			"correct":      true,
		},
	})
	if err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}

	got, err := repo.Get(context.Background(), testIdentity, "expression", "vocab-42")
	if err != nil {
		t.Fatalf("the drilled word was never scheduled: %v", err)
	}
	if got.Successes != 1 {
		t.Errorf("Successes = %d, want 1", got.Successes)
	}
}

// "expression" and the vocabulary id, matching what
// handleVocabularyProducedCorrectly already writes — so a word has ONE
// schedule however it was practised, rather than two that disagree.
func TestADrilledWordSharesItsScheduleWithAProducedOne(t *testing.T) {
	consumer, repo := newConsumerHarness()
	ctx := context.Background()

	if err := consumer.HandleEvent(ctx, event.LearningEvent{
		IdentityID: testIdentity, Type: event.TypeVocabularyProducedCorrectly, Subject: "vocab-42",
	}); err != nil {
		t.Fatalf("produced: %v", err)
	}
	if err := consumer.HandleEvent(ctx, event.LearningEvent{
		IdentityID: testIdentity, Type: event.TypeQuizAnswered, Subject: "ex-1",
		Evidence: map[string]any{"subject_type": "word", "subject_ref": "vocab-42", "correct": true},
	}); err != nil {
		t.Fatalf("drilled: %v", err)
	}

	got, err := repo.Get(ctx, testIdentity, "expression", "vocab-42")
	if err != nil {
		t.Fatal(err)
	}
	if got.Successes != 2 {
		t.Errorf("Successes = %d, want 2 — the two paths wrote to different schedules", got.Successes)
	}
}

// Events written before subject_type existed were all concept drills.
// They must keep scheduling, or upgrading silently drops history.
func TestHandleQuizAnsweredStillHonoursTheOldConceptEvidence(t *testing.T) {
	consumer, repo := newConsumerHarness()

	if err := consumer.HandleEvent(context.Background(), event.LearningEvent{
		IdentityID: testIdentity, Type: event.TypeQuizAnswered, Subject: "ex-1",
		Evidence: map[string]any{"concept": "te-form", "correct": true},
	}); err != nil {
		t.Fatalf("HandleEvent returned error: %v", err)
	}
	if _, err := repo.Get(context.Background(), testIdentity, "concept", "te-form"); err != nil {
		t.Fatalf("a pre-subject_type event stopped scheduling: %v", err)
	}
}
