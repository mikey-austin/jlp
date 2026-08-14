// Package inprocbus is an in-process, synchronous implementation of
// events.EventBus: handlers run on the publishing goroutine, in the order
// they subscribed, and their errors are joined into Publish's return value.
package inprocbus

import (
	"context"
	"errors"
	"sync"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/ports/events"
)

type bus struct {
	mu       sync.RWMutex
	handlers map[event.Type][]events.Handler
}

// New returns an events.EventBus backed by an in-memory subscriber map.
// Safe for concurrent use.
func New() events.EventBus {
	return &bus{handlers: map[event.Type][]events.Handler{}}
}

func (b *bus) Subscribe(t event.Type, h events.Handler) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.handlers[t] = append(b.handlers[t], h)
}

// Publish dispatches ev synchronously to every handler subscribed to
// ev.Type at the time of the call. It copies the subscriber slice under
// the read lock before invoking handlers, so a handler that subscribes
// (e.g. during startup wiring on another goroutine) never races the
// iteration. Publishing a type with no subscribers is a no-op returning
// nil.
func (b *bus) Publish(ctx context.Context, ev event.LearningEvent) error {
	b.mu.RLock()
	subs := b.handlers[ev.Type]
	handlers := make([]events.Handler, len(subs))
	copy(handlers, subs)
	b.mu.RUnlock()

	var errs []error
	for _, h := range handlers {
		if err := h(ctx, ev); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
