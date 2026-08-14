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

// TestPriorityReplaceAllAtomicityAndCrossIdentityIsolation pins the
// brief's Step 2 contract: ReplaceAll's delete+insert must be atomic
// (a second call wholly replaces the first — old rows gone, no
// duplicates), Top must return score-DESC, and both must never leak
// across identities.
func TestPriorityReplaceAllAtomicityAndCrossIdentityIsolation(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-prio-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-prio-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	repo := NewPriorityRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	// Identity B gets its own row first — must survive everything done
	// to identity A below.
	if err := repo.ReplaceAll(ctx, identityB.ID, []storage.Priority{
		{SubjectType: "correction-type", Subject: "conjugation", Score: 9.9, Reason: "identity B's own row", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	// First ReplaceAll for identity A: two rows.
	if err := repo.ReplaceAll(ctx, identityA.ID, []storage.Priority{
		{SubjectType: "correction-type", Subject: "conjugation", Score: 1.0, Reason: "low score", UpdatedAt: now},
		{SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5, Reason: "recurring weakness: 5 occurrences in 30d", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	top, err := repo.Top(ctx, identityA.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("Top(identityA) = %d rows, want 2: %+v", len(top), top)
	}
	// score DESC.
	if top[0].Subject != "i-adjective-past" || top[1].Subject != "conjugation" {
		t.Fatalf("Top(identityA) not score DESC: %+v", top)
	}
	if top[0].Score != 7.5 {
		t.Fatalf("Top(identityA)[0].Score = %v, want 7.5", top[0].Score)
	}
	if top[0].Reason != "recurring weakness: 5 occurrences in 30d" {
		t.Fatalf("Top(identityA)[0].Reason = %q", top[0].Reason)
	}

	// Second ReplaceAll for identity A: a DIFFERENT, single-row set. The
	// old two rows (including "conjugation", untouched by this call)
	// must be gone entirely — this is the atomicity the brief calls out:
	// ReplaceAll is a full swap, not a merge/upsert.
	if err := repo.ReplaceAll(ctx, identityA.ID, []storage.Priority{
		{SubjectType: "concept", Subject: "particle-wa-topic", Score: 3.3, Reason: "second recompute", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	top, err = repo.Top(ctx, identityA.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 1 {
		t.Fatalf("Top(identityA) after second ReplaceAll = %d rows, want 1 (old rows must be gone): %+v", len(top), top)
	}
	if top[0].Subject != "particle-wa-topic" {
		t.Fatalf("Top(identityA)[0].Subject = %q, want particle-wa-topic", top[0].Subject)
	}

	// Identity B's row, untouched by any of identity A's ReplaceAll
	// calls, must still be exactly as originally written.
	topB, err := repo.Top(ctx, identityB.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(topB) != 1 || topB[0].Subject != "conjugation" || topB[0].Score != 9.9 {
		t.Fatalf("Top(identityB) = %+v, want identity B's original untouched row", topB)
	}

	// ReplaceAll with an empty slice must clear every row for the
	// identity, not error or leave stale rows behind.
	if err := repo.ReplaceAll(ctx, identityA.ID, nil); err != nil {
		t.Fatal(err)
	}
	top, err = repo.Top(ctx, identityA.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 0 {
		t.Fatalf("Top(identityA) after empty ReplaceAll = %d rows, want 0: %+v", len(top), top)
	}
}

// TestPriorityTopRespectsLimit pins Top's limit parameter.
func TestPriorityTopRespectsLimit(t *testing.T) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-prio-limit-" + uuid.NewString()), DisplayName: "Limit"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	repo := NewPriorityRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := repo.ReplaceAll(ctx, identity.ID, []storage.Priority{
		{SubjectType: "correction-type", Subject: "a", Score: 1, Reason: "r", UpdatedAt: now},
		{SubjectType: "correction-type", Subject: "b", Score: 2, Reason: "r", UpdatedAt: now},
		{SubjectType: "correction-type", Subject: "c", Score: 3, Reason: "r", UpdatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	top, err := repo.Top(ctx, identity.ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 {
		t.Fatalf("Top(limit=2) = %d rows, want 2: %+v", len(top), top)
	}
	if top[0].Subject != "c" || top[1].Subject != "b" {
		t.Fatalf("Top(limit=2) = %+v, want [c, b] (score DESC, truncated)", top)
	}
}
