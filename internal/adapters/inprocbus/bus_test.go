package inprocbus_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/domain/event"
)

func TestPublishCallsAllSubscribedHandlers(t *testing.T) {
	bus := inprocbus.New()
	var gotA, gotB event.LearningEvent
	var nA, nB int
	bus.Subscribe(event.TypeWritingUpdated, func(_ context.Context, ev event.LearningEvent) error {
		gotA = ev
		nA++
		return nil
	})
	bus.Subscribe(event.TypeWritingUpdated, func(_ context.Context, ev event.LearningEvent) error {
		gotB = ev
		nB++
		return nil
	})

	ev := event.LearningEvent{ID: "ev-1", Type: event.TypeWritingUpdated, Subject: "doc-1"}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if nA != 1 || nB != 1 {
		t.Fatalf("handler call counts = (%d,%d), want (1,1)", nA, nB)
	}
	if gotA.ID != ev.ID || gotB.ID != ev.ID {
		t.Fatalf("handlers did not receive the published event: gotA=%+v gotB=%+v", gotA, gotB)
	}
}

func TestPublishJoinsHandlerErrors(t *testing.T) {
	bus := inprocbus.New()
	errA := errors.New("handler a failed")
	errB := errors.New("handler b failed")
	bus.Subscribe(event.TypeWritingUpdated, func(context.Context, event.LearningEvent) error { return errA })
	bus.Subscribe(event.TypeWritingUpdated, func(context.Context, event.LearningEvent) error { return errB })

	err := bus.Publish(context.Background(), event.LearningEvent{Type: event.TypeWritingUpdated})
	if err == nil {
		t.Fatal("Publish returned nil, want the joined handler errors")
	}
	if !errors.Is(err, errA) || !errors.Is(err, errB) {
		t.Fatalf("Publish err = %v, want it to wrap both handler errors", err)
	}
}

func TestPublishWithNoSubscribersIsNoop(t *testing.T) {
	bus := inprocbus.New()
	err := bus.Publish(context.Background(), event.LearningEvent{Type: event.TypeWritingCreated})
	if err != nil {
		t.Fatalf("Publish with no subscribers returned %v, want nil", err)
	}
}

func TestSubscribersForOneTypeDoNotSeeAnotherType(t *testing.T) {
	bus := inprocbus.New()
	called := false
	bus.Subscribe(event.TypeWritingCreated, func(context.Context, event.LearningEvent) error {
		called = true
		return nil
	})

	if err := bus.Publish(context.Background(), event.LearningEvent{Type: event.TypeWritingUpdated}); err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
	if called {
		t.Fatal("handler subscribed to a different type was invoked")
	}
}
