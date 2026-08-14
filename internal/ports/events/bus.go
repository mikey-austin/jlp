package events

import (
	"context"

	"github.com/mikeyaustin/jlp/internal/domain/event"
)

// Handler reacts to a published learning event. Returning an error does not
// stop other handlers from running; EventBus.Publish joins all handler
// errors and returns them together.
type Handler func(ctx context.Context, ev event.LearningEvent) error

// EventBus dispatches learning events to handlers subscribed by type.
// Dispatch is synchronous: Publish does not return until every subscribed
// handler has run.
type EventBus interface {
	// Publish invokes every handler subscribed to ev.Type, synchronously,
	// and joins their errors. Publishing a type with no subscribers is a
	// no-op that returns nil.
	Publish(ctx context.Context, ev event.LearningEvent) error
	// Subscribe registers h to be invoked for every future Publish of type t.
	Subscribe(t event.Type, h Handler)
}
