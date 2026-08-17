// Package learning is the single write path for learning events: every
// later producer (writing, feedback, corrections) records through a
// Recorder rather than touching storage.LearningEventRepository or
// events.EventBus directly, so the append-before-publish ordering and
// ID/timestamp defaulting stay in one place.
package learning

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/events"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

type Recorder struct {
	store storage.LearningEventRepository
	bus   events.EventBus
}

func NewRecorder(store storage.LearningEventRepository, bus events.EventBus) *Recorder {
	return &Recorder{store: store, bus: bus}
}

// Record fills ev's ID and OccurredAt when they're zero, appends it to the
// store, then publishes it on the bus. The append and the publish are
// ordered deliberately: an event is durable before anything reacts to it.
//
// Record's durability contract: it returns an error only when the event
// failed to become durable, i.e. when Append fails — in that case Record
// returns without publishing. Once the event is durably appended, Record
// always returns nil. A Publish error means a downstream reactor failed
// to react to an already-durable event, not that the event was lost, so
// it is logged rather than returned: producers that call Record (writing,
// feedback, corrections) must never fail because a subscriber errored.
func (r *Recorder) Record(ctx context.Context, ev event.LearningEvent) error {
	if ev.ID == "" {
		ev.ID = uuid.NewString()
	}
	if ev.OccurredAt.IsZero() {
		ev.OccurredAt = time.Now().UTC()
	}

	if err := r.store.Append(ctx, ev); err != nil {
		return err
	}
	if err := r.bus.Publish(ctx, ev); err != nil {
		slog.Error("event publish", "type", ev.Type, "err", err)
	}
	return nil
}

// Kind names what a learner deleted or restored, for
// RecordDeletion/RecordRestore's Evidence. All three ids are bare
// UUIDs, so the kind cannot be inferred from the event's Subject and
// has to be carried explicitly.
type Kind string

const (
	KindSession    Kind = "session"
	KindVocabulary Kind = "vocabulary"
	KindLesson     Kind = "lesson"
)

// RecordDeletion appends the content.deleted event for a soft delete
// (Phase 4 Task D). It exists so the three delete paths — sessions,
// vocabulary items, lessons — cannot each invent their own spelling of
// the same event: one Evidence shape, one Type, defined once. See
// event.TypeContentDeleted for why a soft delete records an event at
// all.
//
// SessionID is deliberately left nil even when kind is KindSession:
// filing a session's deletion inside that same session would put the
// record inside the thing it is a record of.
func (r *Recorder) RecordDeletion(ctx context.Context, identity learner.IdentityID, kind Kind, subject string) error {
	return r.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeContentDeleted,
		Subject:    subject,
		Evidence:   map[string]any{"kind": string(kind)},
	})
}

// RecordRestore appends the content.restored event — the mirror of
// RecordDeletion, same Evidence shape.
func (r *Recorder) RecordRestore(ctx context.Context, identity learner.IdentityID, kind Kind, subject string) error {
	return r.Record(ctx, event.LearningEvent{
		IdentityID: identity,
		Type:       event.TypeContentRestored,
		Subject:    subject,
		Evidence:   map[string]any{"kind": string(kind)},
	})
}
