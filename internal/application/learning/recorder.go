// Package learning is the single write path for learning events: every
// later producer (writing, feedback, corrections) records through a
// Recorder rather than touching storage.LearningEventRepository or
// events.EventBus directly, so the append-before-publish ordering and
// ID/timestamp defaulting stay in one place.
package learning

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/event"
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
// If Append fails, Record returns that error without publishing. If
// Publish fails, Record still returns the error, but the appended row
// stays — events are never rolled back once written.
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
	return r.bus.Publish(ctx, ev)
}
