package sessions_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Soft delete for sessions (Phase 4 Task D). The fake this drives
// honours deleted_at the way the real adapter's SQL does — see
// fakeSessionRepo in service_test.go — so "it stops coming back from
// reads" is actually asserted here rather than assumed.

func TestDeleteHidesSessionFromGetAndList(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if _, err := svc.Get(ctx, "learner-a", created.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Get after Delete = %v, want storage.ErrNotFound", err)
	}
	list, err := svc.List(ctx, "learner-a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("List after Delete returned %d sessions, want 0", len(list))
	}
}

// The whole point of soft delete: the row is marked, not removed. A
// test that only checked Get/List could not tell the two apart.
func TestDeleteMarksRatherThanRemoves(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if !repo.exists("learner-a", created.ID) {
		t.Fatal("the session row is gone from storage; soft delete must mark it, never remove it")
	}
	if !repo.isDeleted("learner-a", created.ID) {
		t.Fatal("the session row is not marked deleted")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("first Delete returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("second Delete returned error: %v — deleting an already-deleted session must be a success", err)
	}
}

func TestDeleteUnknownSessionReturnsErrNotFound(t *testing.T) {
	svc := newService(newFakeSessionRepo())

	err := svc.Delete(context.Background(), "learner-a", "no-such-session")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Delete of unknown id = %v, want storage.ErrNotFound", err)
	}
}

// The user's explicit ask. Two assertions, and the second is the one
// that matters: "returns ErrNotFound" alone would pass even if the row
// had been deleted anyway.
func TestDeleteForAnotherIdentityMissesAndLeavesTheRowIntact(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}

	err = svc.Delete(ctx, "learner-b", created.ID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Delete = %v, want storage.ErrNotFound (indistinguishable from an unknown id)", err)
	}

	if repo.isDeleted("learner-a", created.ID) {
		t.Fatal("learner-b's delete marked learner-a's session deleted")
	}
	got, err := svc.Get(ctx, "learner-a", created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("owner's Get after a cross-identity delete = (%v, %v), want the session back intact", got.ID, err)
	}
}

func TestRestoreBringsTheSessionBack(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := svc.Restore(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	if _, err := svc.Get(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Get after Restore returned error: %v", err)
	}
	list, err := svc.List(ctx, "learner-a")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List after Restore returned %d sessions, want 1", len(list))
	}
}

func TestRestoreForAnotherIdentityMisses(t *testing.T) {
	repo := newFakeSessionRepo()
	svc := newService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if err := svc.Restore(ctx, "learner-b", created.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Restore = %v, want storage.ErrNotFound", err)
	}
	if !repo.isDeleted("learner-a", created.ID) {
		t.Fatal("learner-b's restore un-deleted learner-a's session")
	}
}

func TestDeleteAndRestoreRecordLearningEvents(t *testing.T) {
	repo := newFakeSessionRepo()
	svc, store := newTestService(repo)
	ctx := context.Background()

	created, err := svc.Create(ctx, "learner-a", "A's session", "Diary", session.Profile{})
	if err != nil {
		t.Fatalf("Create returned error: %v", err)
	}
	if err := svc.Delete(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := svc.Restore(ctx, "learner-a", created.ID); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	if len(store.appended) != 2 {
		t.Fatalf("appended %d events, want 2 (content.deleted then content.restored)", len(store.appended))
	}
	if got := store.appended[0].Type; got != event.TypeContentDeleted {
		t.Fatalf("first event Type = %q, want %q", got, event.TypeContentDeleted)
	}
	if got := store.appended[1].Type; got != event.TypeContentRestored {
		t.Fatalf("second event Type = %q, want %q", got, event.TypeContentRestored)
	}
	for _, ev := range store.appended {
		if ev.Subject != string(created.ID) {
			t.Fatalf("%s Subject = %q, want the session id %q", ev.Type, ev.Subject, created.ID)
		}
		if ev.IdentityID != "learner-a" {
			t.Fatalf("%s IdentityID = %q, want learner-a", ev.Type, ev.IdentityID)
		}
		if ev.Evidence["kind"] != "session" {
			t.Fatalf("%s Evidence[kind] = %v, want %q", ev.Type, ev.Evidence["kind"], "session")
		}
		// A deletion is scoped to the identity that made it, not filed
		// inside the session it removes.
		if ev.SessionID != nil {
			t.Fatalf("%s SessionID = %v, want nil", ev.Type, *ev.SessionID)
		}
	}
}
