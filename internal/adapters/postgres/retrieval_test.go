//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestRetrievalUpsertReplacesInPlaceAndIsolatesByIdentity(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-ret-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-ret-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	repo := NewRetrievalRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	original := storage.RetrievalItem{
		IdentityID:  identityA.ID,
		SubjectType: "concept",
		Subject:     "te-form",
		Successes:   1,
		Failures:    0,
		LastSeen:    now,
		DueAt:       now.Add(24 * time.Hour),
		Interval:    24 * time.Hour,
		Confidence:  0,
	}
	if err := repo.Upsert(ctx, original); err != nil {
		t.Fatal(err)
	}

	// A second row for identity B on the SAME subject_type/subject must
	// not collide with identity A's row (the PRIMARY KEY is scoped by
	// identity_id) and must never show up in identity A's reads.
	otherIdentity := original
	otherIdentity.IdentityID = identityB.ID
	if err := repo.Upsert(ctx, otherIdentity); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Get(ctx, identityA.ID, "concept", "te-form")
	if err != nil {
		t.Fatal(err)
	}
	if got.Successes != 1 || got.Interval != 24*time.Hour {
		t.Fatalf("Get after first upsert = %+v, want Successes=1 Interval=24h", got)
	}

	// Get for identityB's OWN row on the same subject must succeed
	// (proves the PK is per-identity, not a global unique on
	// subject_type/subject).
	if _, err := repo.Get(ctx, identityB.ID, "concept", "te-form"); err != nil {
		t.Fatalf("identity B's own row missed: %v", err)
	}

	// A cross-identity Get (identity A asking for identity B's subject,
	// which doesn't exist for A) must miss with ErrNotFound, never
	// return B's row.
	if _, err := repo.Get(ctx, identityA.ID, "concept", "nonexistent-for-a"); err == nil {
		t.Fatal("Get for an unscheduled subject returned nil error, want ErrNotFound")
	} else if err != storage.ErrNotFound {
		t.Fatalf("Get error = %v, want storage.ErrNotFound", err)
	}

	// Upsert again for the SAME (identity, subject_type, subject): must
	// REPLACE in place (same row), not insert a second one.
	later := now.Add(3 * time.Hour)
	replacement := original
	replacement.Successes = 2
	replacement.LastSeen = later
	replacement.DueAt = later.Add(3 * 24 * time.Hour)
	replacement.Interval = 3 * 24 * time.Hour
	replacement.Confidence = 4
	if err := repo.Upsert(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	got, err = repo.Get(ctx, identityA.ID, "concept", "te-form")
	if err != nil {
		t.Fatal(err)
	}
	if got.Successes != 2 {
		t.Errorf("Successes = %d, want 2 (replaced in place)", got.Successes)
	}
	if got.Interval != 3*24*time.Hour {
		t.Errorf("Interval = %v, want 3d", got.Interval)
	}
	if !got.DueAt.Equal(replacement.DueAt) {
		t.Errorf("DueAt = %v, want %v", got.DueAt, replacement.DueAt)
	}
	if got.Confidence != 4 {
		t.Errorf("Confidence = %v, want 4", got.Confidence)
	}

	list, err := repo.List(ctx, identityA.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("identity A List = %d items, want 1 (upsert must not duplicate rows, identity B's row must not leak in)", len(list))
	}
}

// TestRetrievalDueFiltersByTimeAndOrdersEarliestFirst pins Due's
// contract: only items whose DueAt is at or before the query time come
// back, ordered earliest-due first, and a limit caps the result.
func TestRetrievalDueFiltersByTimeAndOrdersEarliestFirst(t *testing.T) {
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	identities := NewIdentityRepository(pool)
	identity := learner.Identity{ID: learner.IdentityID("test-ret-due-" + uuid.NewString()), DisplayName: "Due"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	repo := NewRetrievalRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	seed := func(subject string, dueAt time.Time) {
		if err := repo.Upsert(ctx, storage.RetrievalItem{
			IdentityID:  identity.ID,
			SubjectType: "concept",
			Subject:     subject,
			LastSeen:    now,
			DueAt:       dueAt,
			Interval:    24 * time.Hour,
		}); err != nil {
			t.Fatalf("seed %s: %v", subject, err)
		}
	}
	seed("overdue-2d", now.Add(-48*time.Hour))
	seed("overdue-1d", now.Add(-24*time.Hour))
	seed("due-now", now)
	seed("not-due-yet", now.Add(48*time.Hour))

	due, err := repo.Due(ctx, identity.ID, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 3 {
		t.Fatalf("Due returned %d items, want 3 (not-due-yet must be excluded): %+v", len(due), due)
	}
	wantOrder := []string{"overdue-2d", "overdue-1d", "due-now"}
	for i, w := range wantOrder {
		if due[i].Subject != w {
			t.Fatalf("Due[%d].Subject = %q, want %q (earliest-due first): got order %v", i, due[i].Subject, w, subjectsOf(due))
		}
	}

	limited, err := repo.Due(ctx, identity.ID, now, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 2 {
		t.Fatalf("Due with limit=2 returned %d items, want 2", len(limited))
	}
	if limited[0].Subject != "overdue-2d" || limited[1].Subject != "overdue-1d" {
		t.Fatalf("Due with limit=2 = %v, want the two MOST overdue", subjectsOf(limited))
	}

	// List (unlike Due) returns every scheduled item regardless of
	// DueAt, still earliest-due first.
	all, err := repo.List(ctx, identity.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("List returned %d items, want 4 (List is unfiltered by due time)", len(all))
	}
	if all[len(all)-1].Subject != "not-due-yet" {
		t.Fatalf("List order = %v, want not-due-yet last (still due-at ascending)", subjectsOf(all))
	}
}

func subjectsOf(items []storage.RetrievalItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.Subject
	}
	return out
}
