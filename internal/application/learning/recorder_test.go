package learning_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/application/learning"
	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/events"
)

// fakeEventStore is an in-memory storage.LearningEventRepository. It
// appends to the shared `order` slice so tests can assert Append runs
// before Publish.
type fakeEventStore struct {
	order     *[]string
	appended  []event.LearningEvent
	appendErr error
}

func (f *fakeEventStore) Append(_ context.Context, ev event.LearningEvent) error {
	if f.appendErr != nil {
		return f.appendErr
	}
	*f.order = append(*f.order, "append")
	f.appended = append(f.appended, ev)
	return nil
}

func (f *fakeEventStore) ListRecent(context.Context, learner.IdentityID, *session.ID, int) ([]event.LearningEvent, error) {
	return f.appended, nil
}

// fakeBus is an in-memory events.EventBus that records Publish into the
// same shared `order` slice as fakeEventStore.
type fakeBus struct {
	order      *[]string
	published  []event.LearningEvent
	publishErr error
}

func (f *fakeBus) Publish(_ context.Context, ev event.LearningEvent) error {
	*f.order = append(*f.order, "publish")
	f.published = append(f.published, ev)
	return f.publishErr
}

func (f *fakeBus) Subscribe(event.Type, events.Handler) {}

func TestRecordFillsIDAndOccurredAtWhenZero(t *testing.T) {
	order := []string{}
	store := &fakeEventStore{order: &order}
	bus := &fakeBus{order: &order}
	rec := learning.NewRecorder(store, bus)

	ev := event.LearningEvent{IdentityID: "learner-a", Type: event.TypeWritingUpdated, Subject: "doc-1"}
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if len(store.appended) != 1 {
		t.Fatalf("expected 1 appended event, got %d", len(store.appended))
	}
	got := store.appended[0]
	if got.ID == "" {
		t.Fatal("Record did not fill ID")
	}
	if got.OccurredAt.IsZero() {
		t.Fatal("Record did not fill OccurredAt")
	}
	if len(bus.published) != 1 || bus.published[0].ID != got.ID {
		t.Fatal("Record did not publish the same event it appended")
	}
}

func TestRecordDoesNotOverwriteProvidedIDAndOccurredAt(t *testing.T) {
	order := []string{}
	store := &fakeEventStore{order: &order}
	bus := &fakeBus{order: &order}
	rec := learning.NewRecorder(store, bus)

	ev := event.LearningEvent{ID: "fixed-id", Type: event.TypeWritingUpdated}
	if err := rec.Record(context.Background(), ev); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if store.appended[0].ID != "fixed-id" {
		t.Fatalf("Record overwrote a caller-provided ID: got %q", store.appended[0].ID)
	}
}

func TestRecordAppendsBeforePublishing(t *testing.T) {
	order := []string{}
	store := &fakeEventStore{order: &order}
	bus := &fakeBus{order: &order}
	rec := learning.NewRecorder(store, bus)

	if err := rec.Record(context.Background(), event.LearningEvent{Type: event.TypeWritingUpdated}); err != nil {
		t.Fatalf("Record returned error: %v", err)
	}
	if len(order) != 2 || order[0] != "append" || order[1] != "publish" {
		t.Fatalf("order = %v, want [append publish]", order)
	}
}

func TestRecordPropagatesStoreErrorsWithoutPublishing(t *testing.T) {
	order := []string{}
	wantErr := errors.New("store exploded")
	store := &fakeEventStore{order: &order, appendErr: wantErr}
	bus := &fakeBus{order: &order}
	rec := learning.NewRecorder(store, bus)

	err := rec.Record(context.Background(), event.LearningEvent{Type: event.TypeWritingUpdated})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Record err = %v, want %v", err, wantErr)
	}
	if len(order) != 0 {
		t.Fatalf("order = %v, want empty (publish must not run after a store error)", order)
	}
}

func TestRecordPropagatesPublishErrorButAppendStays(t *testing.T) {
	order := []string{}
	wantErr := errors.New("publish exploded")
	store := &fakeEventStore{order: &order}
	bus := &fakeBus{order: &order, publishErr: wantErr}
	rec := learning.NewRecorder(store, bus)

	err := rec.Record(context.Background(), event.LearningEvent{Type: event.TypeWritingUpdated})
	if !errors.Is(err, wantErr) {
		t.Fatalf("Record err = %v, want %v", err, wantErr)
	}
	if len(store.appended) != 1 {
		t.Fatalf("appended count = %d, want 1 (the append must stick even though publish failed)", len(store.appended))
	}
}
