//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
)

func TestObservationUpsertReplacesInPlaceAndIsolatesByIdentity(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-obs-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-obs-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	repo := NewObservationRepository(pool)
	firstSeen := time.Now().UTC().Truncate(time.Microsecond)

	original := learnermodel.Observation{
		ID:          uuid.NewString(),
		IdentityID:  identityA.ID,
		Kind:        learnermodel.KindWeakness,
		SubjectType: learnermodel.SubjectCorrectionType,
		Subject:     "conjugation",
		Confidence:  0.6,
		Evidence:    map[string]any{"count": float64(3), "window_days": float64(30)},
		FirstSeen:   firstSeen,
		UpdatedAt:   firstSeen,
	}
	if err := repo.Upsert(ctx, original); err != nil {
		t.Fatal(err)
	}

	// A second observation for identity B on the SAME subject_type/subject
	// must not collide with identity A's row (the UNIQUE constraint is
	// scoped by identity_id) and must never show up in identity A's List.
	otherIdentityObs := learnermodel.Observation{
		ID:          uuid.NewString(),
		IdentityID:  identityB.ID,
		Kind:        learnermodel.KindWeakness,
		SubjectType: learnermodel.SubjectCorrectionType,
		Subject:     "conjugation",
		Confidence:  0.6,
		Evidence:    map[string]any{"count": float64(3), "window_days": float64(30)},
		FirstSeen:   firstSeen,
		UpdatedAt:   firstSeen,
	}
	if err := repo.Upsert(ctx, otherIdentityObs); err != nil {
		t.Fatal(err)
	}

	listA, err := repo.List(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 {
		t.Fatalf("identity A observations = %d, want 1 (identity B's row must not leak in)", len(listA))
	}
	if listA[0].ID != original.ID {
		t.Fatalf("identity A's observation ID = %s, want %s", listA[0].ID, original.ID)
	}

	// Upsert again for the SAME (identity, subject_type, subject): a
	// later re-detection with a new ID and later FirstSeen — the
	// UNIQUE-constrained row must be replaced in place (same DB row,
	// original ID/FirstSeen kept), not duplicated, and Kind/Confidence/
	// Evidence/UpdatedAt must reflect the new call.
	later := firstSeen.Add(2 * time.Hour)
	replacement := learnermodel.Observation{
		ID:          uuid.NewString(), // deliberately a different ID
		IdentityID:  identityA.ID,
		Kind:        learnermodel.KindEmerging,
		SubjectType: learnermodel.SubjectCorrectionType,
		Subject:     "conjugation",
		Confidence:  0.6,
		Evidence:    map[string]any{"count": float64(3), "window_days": float64(30)},
		FirstSeen:   later, // deliberately a different FirstSeen
		UpdatedAt:   later,
	}
	if err := repo.Upsert(ctx, replacement); err != nil {
		t.Fatal(err)
	}

	listA, err = repo.List(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 {
		t.Fatalf("identity A observations after replace = %d, want 1 (upsert must replace, not insert a second row)", len(listA))
	}
	got := listA[0]
	if got.ID != original.ID {
		t.Errorf("ID = %s, want the ORIGINAL id %s (a conflicting upsert must keep the existing row's id)", got.ID, original.ID)
	}
	if !got.FirstSeen.Equal(firstSeen) {
		t.Errorf("FirstSeen = %v, want the ORIGINAL first_seen %v (must not be bumped by a later re-detection)", got.FirstSeen, firstSeen)
	}
	if got.Kind != learnermodel.KindEmerging {
		t.Errorf("Kind = %q, want %q (kind must be replaced)", got.Kind, learnermodel.KindEmerging)
	}
	if !got.UpdatedAt.Equal(later) {
		t.Errorf("UpdatedAt = %v, want %v (updated_at must be replaced)", got.UpdatedAt, later)
	}

	// DeleteAll must be scoped to the identity: deleting A's observations
	// leaves B's row untouched.
	if err := repo.DeleteAll(ctx, identityA.ID); err != nil {
		t.Fatal(err)
	}
	listA, err = repo.List(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 0 {
		t.Fatalf("identity A observations after DeleteAll = %d, want 0", len(listA))
	}
	listB, err := repo.List(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 1 {
		t.Fatalf("identity B observations after A's DeleteAll = %d, want 1 (DeleteAll must not cross identities)", len(listB))
	}
}
