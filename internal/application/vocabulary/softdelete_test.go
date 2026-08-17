package vocabulary_test

import (
	"context"
	"errors"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/event"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Soft delete for vocabulary items (Phase 4 Task D). The fake this
// drives honours deleted_at across List, AllExpressions and
// GetByExpressions the way the real adapter's SQL does — see
// fakeVocabRepo in service_test.go.

// ingestOne looks an expression up once and returns the item.
func ingestOne(t *testing.T, h *harness, expression string) string {
	t.Helper()
	item, err := h.svc.Ingest(context.Background(), testIdentity, lookupEvent(expression))
	if err != nil {
		t.Fatalf("Ingest(%q) returned error: %v", expression, err)
	}
	return item.ID
}

func TestDeleteHidesItemFromEveryRead(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	// The /vocabulary page's four tabs all come through List.
	for _, filter := range []string{"", "looked-up", "produced", "activate"} {
		items, err := h.svc.List(ctx, testIdentity, filter)
		if err != nil {
			t.Fatalf("List(%q) returned error: %v", filter, err)
		}
		for _, item := range items {
			if item.ID == id {
				t.Fatalf("List(%q) still returns the deleted item", filter)
			}
		}
	}

	// GetByExpressions backs the retrieval scheduler's due-expression
	// resolution.
	got, err := h.svc.GetByExpressions(ctx, testIdentity, []string{"取り組む"})
	if err != nil {
		t.Fatalf("GetByExpressions returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetByExpressions returned %d items, want 0 for a deleted expression", len(got))
	}

	// DetectProduction scans AllExpressions. A deleted word left in
	// that set would keep collecting production events from a row the
	// learner cannot see.
	if err := h.svc.DetectProduction(ctx, testIdentity, testSessionID, "今日は新しい仕事に取り組むつもりです。", nil); err != nil {
		t.Fatalf("DetectProduction returned error: %v", err)
	}
	for _, call := range h.repo.productions {
		if call.ItemID == id {
			t.Fatal("DetectProduction recorded a production against a deleted item")
		}
	}
}

func TestDeleteMarksRatherThanRemoves(t *testing.T) {
	h := newHarness()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(context.Background(), testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}

	if !h.repo.exists(id) {
		t.Fatal("the vocabulary row is gone from storage; soft delete must mark it, never remove it")
	}
	if !h.repo.isDeleted(id) {
		t.Fatal("the vocabulary row is not marked deleted")
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("first Delete returned error: %v", err)
	}
	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("second Delete returned error: %v — deleting an already-deleted item must be a success", err)
	}
}

func TestDeleteUnknownItemReturnsErrNotFound(t *testing.T) {
	h := newHarness()
	if err := h.svc.Delete(context.Background(), testIdentity, "no-such-item"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Delete of unknown id = %v, want storage.ErrNotFound", err)
	}
}

// The user's explicit ask. The second assertion is the one that
// matters: "returns ErrNotFound" alone would pass even if the row had
// been deleted anyway.
func TestDeleteForAnotherIdentityMissesAndLeavesTheRowIntact(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, "learner-b", id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Delete = %v, want storage.ErrNotFound (indistinguishable from an unknown id)", err)
	}

	if h.repo.isDeleted(id) {
		t.Fatal("learner-b's delete marked learner-a's vocabulary item deleted")
	}
	items, err := h.svc.List(ctx, testIdentity, "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 || items[0].ID != id {
		t.Fatalf("owner's List after a cross-identity delete returned %d items, want the item back intact", len(items))
	}
}

func TestRestoreBringsTheItemBack(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, testIdentity, id); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	items, err := h.svc.List(ctx, testIdentity, "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 || items[0].ID != id {
		t.Fatalf("List after Restore returned %d items, want the item back", len(items))
	}
}

func TestRestoreForAnotherIdentityMisses(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, "learner-b", id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Restore = %v, want storage.ErrNotFound", err)
	}
	if !h.repo.isDeleted(id) {
		t.Fatal("learner-b's restore un-deleted learner-a's vocabulary item")
	}
}

// A deleted word that the learner deliberately looks up again comes
// back. UNIQUE (identity_id, expression) means the lookup lands on the
// hidden row either way; the choice is between it reappearing (visible,
// and re-deletable) and the lookup vanishing into a row they cannot
// reach (invisible). See db/queries/vocabulary.sql's
// UpsertVocabularyItemOnLookup.
func TestLookingADeletedWordUpAgainResurrectsIt(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if again := ingestOne(t, h, "取り組む"); again != id {
		t.Fatalf("re-lookup created a new item %q, want the same row %q back", again, id)
	}

	items, err := h.svc.List(ctx, testIdentity, "")
	if err != nil {
		t.Fatalf("List returned error: %v", err)
	}
	if len(items) != 1 || items[0].ID != id {
		t.Fatalf("List after a re-lookup returned %d items, want the resurrected item", len(items))
	}
}

func TestDeleteAndRestoreRecordLearningEvents(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	id := ingestOne(t, h, "取り組む")
	before := len(h.events.events)

	if err := h.svc.Delete(ctx, testIdentity, id); err != nil {
		t.Fatalf("Delete returned error: %v", err)
	}
	if err := h.svc.Restore(ctx, testIdentity, id); err != nil {
		t.Fatalf("Restore returned error: %v", err)
	}

	got := h.events.events[before:]
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
		if ev.Subject != id {
			t.Fatalf("%s Subject = %q, want the item id %q", ev.Type, ev.Subject, id)
		}
		if ev.IdentityID != testIdentity {
			t.Fatalf("%s IdentityID = %q, want %q", ev.Type, ev.IdentityID, testIdentity)
		}
		if ev.Evidence["kind"] != "vocabulary" {
			t.Fatalf("%s Evidence[kind] = %v, want %q", ev.Type, ev.Evidence["kind"], "vocabulary")
		}
	}
}
