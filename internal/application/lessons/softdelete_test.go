package lessons_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Soft delete for lesson guides (Phase 4 Task D). The fake this drives
// honours deleted_at across List, Get, Observations and
// CompleteWithObservation the way the real adapter's SQL does — see
// fakeLessonRepo in service_test.go.

func generateOne(t *testing.T, h *testHarness) storage.Lesson {
	t.Helper()
	l, err := h.svc.Generate(context.Background(), "learner-a")
	if err != nil {
		t.Fatalf("Generate returned error: %v", err)
	}
	return l
}

func TestDeleteHidesLessonFromListGetAndObservations(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if _, err := h.svc.Complete(ctx, "learner-a", l.ID, "tutor", "notes", []string{"te-form"}); err != nil {
		t.Fatalf("Complete returned error: %v", err)
	}
	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	list, err := h.lessons.List(ctx, "learner-a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List after Delete returned %d lessons, want 0", len(list))
	}
	if _, err := h.lessons.Get(ctx, "learner-a", l.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want storage.ErrNotFound", err)
	}
	obs, err := h.lessons.Observations(ctx, "learner-a", l.ID)
	if err != nil {
		t.Fatalf("Observations returned error: %v", err)
	}
	if len(obs) != 0 {
		t.Fatalf("Observations after Delete returned %d, want 0 — a deleted lesson's tutor notes must go with it", len(obs))
	}
}

// A deleted lesson is unreachable, so it must not be completable
// either: an observation attached to it would be durably stored yet
// permanently invisible.
func TestCompleteOnADeletedLessonMisses(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if _, err := h.svc.Complete(ctx, "learner-a", l.ID, "tutor", "notes", nil); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Complete on a deleted lesson = %v, want storage.ErrNotFound", err)
	}
}

func TestDeleteMarksRatherThanRemoves(t *testing.T) {
	h := newTestHarness()
	l := generateOne(t, h)

	if err := h.svc.Delete(context.Background(), "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if !h.lessons.exists(l.ID) {
		t.Fatal("the lesson row is gone from storage; soft delete must mark it, never remove it")
	}
	if !h.lessons.isDeleted(l.ID) {
		t.Fatal("the lesson row is not marked deleted")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("first Delete returned error: %v", err)
	}
	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("second Delete returned error: %v — deleting an already-deleted lesson must be a success", err)
	}
}

func TestDeleteUnknownLessonReturnsErrNotFound(t *testing.T) {
	h := newTestHarness()
	if err := h.svc.Delete(context.Background(), "learner-a", "no-such-lesson"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Delete of unknown id = %v, want storage.ErrNotFound", err)
	}
}

// The user's explicit ask. The second assertion is the one that
// matters: "returns ErrNotFound" alone would pass even if the row had
// been deleted anyway.
func TestDeleteForAnotherIdentityMissesAndLeavesTheRowIntact(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if err := h.svc.Delete(ctx, "learner-b", l.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Delete = %v, want storage.ErrNotFound (indistinguishable from an unknown id)", err)
	}

	if h.lessons.isDeleted(l.ID) {
		t.Fatal("learner-b's delete marked learner-a's lesson deleted")
	}
	if _, err := h.lessons.Get(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("owner's Get after a cross-identity delete = %v, want the lesson back intact", err)
	}
}

func TestRestoreBringsTheLessonBack(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	list, err := h.lessons.List(ctx, "learner-a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 || list[0].ID != l.ID {
		t.Fatalf("List after Restore returned %d lessons, want the lesson back", len(list))
	}
}

func TestRestoreForAnotherIdentityMisses(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)

	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, "learner-b", l.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Restore = %v, want storage.ErrNotFound", err)
	}
	if !h.lessons.isDeleted(l.ID) {
		t.Fatal("learner-b's restore un-deleted learner-a's lesson")
	}
}

func TestDeleteAndRestoreRecordLearningEvents(t *testing.T) {
	h := newTestHarness()
	ctx := context.Background()
	l := generateOne(t, h)
	before := len(h.events.appended)

	if err := h.svc.Delete(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, "learner-a", l.ID); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	got := h.events.appended[before:]
	if len(got) != 2 {
		t.Fatalf("appended %d events, want 2 (content.deleted then content.restored)", len(got))
	}
	if got[0].Type != event.TypeContentDeleted {
		t.Fatalf("first event Type = %q, want %q", got[0].Type, event.TypeContentDeleted)
	}
	if got[1].Type != event.TypeContentRestored {
		t.Fatalf("second event Type = %q, want %q", got[1].Type, event.TypeContentRestored)
	}
	for _, ev := range got {
		if ev.Subject != l.ID {
			t.Fatalf("%s Subject = %q, want the lesson id %q", ev.Type, ev.Subject, l.ID)
		}
		if ev.IdentityID != "learner-a" {
			t.Fatalf("%s IdentityID = %q, want learner-a", ev.Type, ev.IdentityID)
		}
		if ev.Evidence["kind"] != "lesson" {
			t.Fatalf("%s Evidence[kind] = %v, want %q", ev.Type, ev.Evidence["kind"], "lesson")
		}
	}
}
