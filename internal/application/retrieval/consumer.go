package retrieval

import (
	"context"
	"fmt"

	"github.com/mikeyaustin/jlp/internal/domain/event"
)

// Consumer is the event-bus side of spaced retrieval (PRD §54):
// HandleEvent reacts to the three outcome events the brief wires this
// from — quiz.answered, correction.retried, vocabulary.produced-
// correctly — folding each into Scheduler.RecordOutcome, the same
// "constructor takes its collaborators, HandleEvent is an
// events.Handler switching on ev.Type" shape application/learnermodel's
// Updater uses (see that package's consumers.go). Any other event type
// is a no-op — defensive: only these three are ever subscribed to
// HandleEvent in cmd/jlp/main.go.
type Consumer struct {
	scheduler *Scheduler
}

// NewConsumer builds a Consumer over scheduler.
func NewConsumer(scheduler *Scheduler) *Consumer {
	return &Consumer{scheduler: scheduler}
}

// HandleEvent is the events.Handler main.go subscribes to
// quiz.answered/correction.retried/vocabulary.produced-correctly.
func (c *Consumer) HandleEvent(ctx context.Context, ev event.LearningEvent) error {
	switch ev.Type {
	case event.TypeQuizAnswered:
		return c.handleQuizAnswered(ctx, ev)
	case event.TypeCorrectionRetried:
		return c.handleCorrectionRetried(ctx, ev)
	case event.TypeVocabularyProducedCorrectly:
		return c.handleVocabularyProducedCorrectly(ctx, ev)
	default:
		return nil
	}
}

// handleQuizAnswered schedules the exercise's concept (Evidence
// "concept") using the reported correct/confidence — quiz.answered's
// Evidence always carries both (application/practice.Service.Answer).
// A missing/empty concept is a no-op: nothing to schedule.
func (c *Consumer) handleQuizAnswered(ctx context.Context, ev event.LearningEvent) error {
	concept, _ := ev.Evidence["concept"].(string)
	if concept == "" {
		return nil
	}
	correct, _ := ev.Evidence["correct"].(bool)
	confidence := evidenceInt(ev.Evidence, "confidence")
	if err := c.scheduler.RecordOutcome(ctx, ev.IdentityID, "concept", concept, correct, confidence); err != nil {
		return fmt.Errorf("retrieval: handle %s: %w", event.TypeQuizAnswered, err)
	}
	return nil
}

// handleCorrectionRetried schedules every RESOLVED concept tagged to
// the retried correction — carried directly in Evidence["concepts"]
// (application/feedback.Service.RetryCorrection fetches them once, via
// storage.FeedbackRepository.GetCorrectionConcepts, and threads them
// through the SAME event this handler reacts to, rather than this
// package re-querying them: two DB round trips for one correction ID
// would otherwise happen on every retry, one from RetryCorrection
// itself and one from here). Per the brief, "independent solve =
// success":
//
//   - correct AND independent (recalled without ever revealing the
//     answer) -> success.
//   - NOT correct -> failure. RetryCorrection's independent is only
//     ever true alongside correct=true, so this is also exactly
//     "independent == false" here.
//   - correct but NOT independent (the answer was revealed, then
//     correctly retyped) -> no update at all: neither a genuine recall
//     success nor a failure, so scheduling it either way would
//     misrepresent what happened — matches domain/correction.IsGated's
//     own refusal to treat a revealed answer as equivalent to
//     independent recall.
//
// A correction with no resolved concepts (an unresolved/hallucinated
// tag, or none at all) is a no-op: there is nothing to schedule.
// correction.retried carries no confidence, so confidence is always 0
// (unknown) here.
func (c *Consumer) handleCorrectionRetried(ctx context.Context, ev event.LearningEvent) error {
	correct, _ := ev.Evidence["correct"].(bool)
	independent, _ := ev.Evidence["independent"].(bool)
	if correct && !independent {
		return nil
	}

	for _, slug := range evidenceStringSlice(ev.Evidence, "concepts") {
		if err := c.scheduler.RecordOutcome(ctx, ev.IdentityID, "concept", slug, correct, 0); err != nil {
			return fmt.Errorf("retrieval: handle %s: %w", event.TypeCorrectionRetried, err)
		}
	}
	return nil
}

// handleVocabularyProducedCorrectly schedules the produced expression
// (ev.Subject) as a success — the event only ever fires when production
// was correct (application/vocabulary.Service.DetectProduction), so
// there is no failure case to distinguish here. Carries no confidence.
func (c *Consumer) handleVocabularyProducedCorrectly(ctx context.Context, ev event.LearningEvent) error {
	if ev.Subject == "" {
		return nil
	}
	if err := c.scheduler.RecordOutcome(ctx, ev.IdentityID, "expression", ev.Subject, true, 0); err != nil {
		return fmt.Errorf("retrieval: handle %s: %w", event.TypeVocabularyProducedCorrectly, err)
	}
	return nil
}

// evidenceInt extracts an int-valued Evidence field, tolerating both the
// live in-process bus's raw Go int (application/*.Service.Record calls
// build Evidence maps with literal ints) and a jsonb round trip's
// float64 (a stored event read back and replayed). Mirrors
// application/planner's evidenceCount, duplicated rather than imported
// for the same no-cross-package-coupling reason that function's own doc
// comment gives.
func evidenceInt(evidence map[string]any, key string) int {
	switch v := evidence[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

// evidenceStringSlice extracts a []string-valued Evidence field, same
// dual-shape tolerance as evidenceInt above: the live in-process bus
// hands handleCorrectionRetried the literal []string
// feedback.Service.RetryCorrection built — application/learning.
// Recorder.Record appends ev to storage first, then calls
// bus.Publish(ctx, ev) with that SAME in-memory ev value, never one
// re-read back from storage, so Evidence is still a real []string, not
// something a jsonb round trip has already turned into []any. A []any
// is tolerated anyway — a JSON array always decodes to []any/
// interface{} elements — for any future caller that replays a STORED
// event (main.go's live subscription is the only caller of this
// package's Consumer today; nothing here assumes it stays that way).
func evidenceStringSlice(evidence map[string]any, key string) []string {
	switch v := evidence[key].(type) {
	case []string:
		return v
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			if s, ok := e.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}
