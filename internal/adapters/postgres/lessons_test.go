//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// decodeJSON unmarshals raw into a generic map for semantic (not
// byte-for-byte) JSON comparison: postgres' jsonb column type
// canonicalizes key order and whitespace on write, so Plan's bytes
// after a round trip are not expected to match the bytes handed to
// Insert verbatim — only their decoded content is.
func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decodeJSON: %v (%s)", err, raw)
	}
	return m
}

// lessonTestSetup migrates and returns a repo plus a freshly-upserted
// identity — same "one throwaway identity per test" pattern
// ankiTestSetup uses.
func lessonTestSetup(t *testing.T) (*LessonRepository, learner.IdentityID) {
	t.Helper()
	ctx := context.Background()
	url := testURL(t)
	if err := Migrate(ctx, url); err != nil {
		t.Fatal(err)
	}
	pool, err := NewPool(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	identities := NewIdentityRepository(pool)
	identity := learner.Identity{ID: learner.IdentityID("test-lesson-" + uuid.NewString()), DisplayName: "Lesson"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}
	return NewLessonRepository(pool), identity.ID
}

func testLesson(identity learner.IdentityID, now time.Time) storage.Lesson {
	return storage.Lesson{
		ID:         uuid.NewString(),
		IdentityID: identity,
		Plan:       []byte(`{"level_summary":"s","strengths":["a"],"weaknesses":["b"],"focus":["c"],"vocabulary":["d"],"grammar_concepts":["e"],"conversation_prompts":["f"],"exercises":["g"],"recent_examples":["h"],"questions_for_tutor":["i"]}`),
		Status:     "prepared",
		CreatedAt:  now,
	}
}

// TestLessonInsertThenGetRoundTrips pins Insert/Get's full-field round
// trip, including the raw Plan JSON surviving byte-for-byte.
func TestLessonInsertThenGetRoundTrips(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	lesson := testLesson(identity, now)

	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := repo.Get(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != lesson.ID {
		t.Fatalf("got.ID = %q, want %q", got.ID, lesson.ID)
	}
	if !reflect.DeepEqual(decodeJSON(t, got.Plan), decodeJSON(t, lesson.Plan)) {
		t.Fatalf("Plan = %s, want (semantically) %s", got.Plan, lesson.Plan)
	}
	if got.Status != "prepared" {
		t.Fatalf("Status = %q, want prepared", got.Status)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if !got.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt = %v, want zero (not yet completed)", got.CompletedAt)
	}
}

// TestLessonGetCrossIdentityMisses pins the identity-scoping contract.
func TestLessonGetCrossIdentityMisses(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	lesson := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Get(ctx, learner.IdentityID("someone-else"), lesson.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Get err = %v, want storage.ErrNotFound", err)
	}
}

// TestLessonGetUnknownIDMisses pins the plain not-found case.
func TestLessonGetUnknownIDMisses(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	_, err := repo.Get(context.Background(), identity, uuid.NewString())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestLessonListNewestFirst pins List's ordering.
func TestLessonListNewestFirst(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	older := testLesson(identity, time.Now().UTC().Add(-time.Hour))
	newer := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, older); err != nil {
		t.Fatal(err)
	}
	if err := repo.Insert(ctx, newer); err != nil {
		t.Fatal(err)
	}

	got, err := repo.List(ctx, identity)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 || got[0].ID != newer.ID || got[1].ID != older.ID {
		t.Fatalf("List = %+v, want [%s, %s] (newest first)", got, newer.ID, older.ID)
	}
}

// TestLessonCompleteWithObservationRoundTrips pins CompleteWithObservation's
// combined status-transition + observation-attach round trip, including
// Subjects surviving the jsonb round trip.
func TestLessonCompleteWithObservationRoundTrips(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	lesson := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	obs := storage.LessonObservation{
		ID:        uuid.NewString(),
		LessonID:  lesson.ID,
		Author:    "tutor-a",
		Notes:     "助詞の復習が必要",
		Subjects:  []string{"i-adjective-past"},
		CreatedAt: now,
	}
	got, err := repo.CompleteWithObservation(ctx, identity, lesson.ID, obs, now)
	if err != nil {
		t.Fatalf("CompleteWithObservation: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("Status = %q, want completed", got.Status)
	}
	if !got.CompletedAt.Equal(now) {
		t.Fatalf("CompletedAt = %v, want %v", got.CompletedAt, now)
	}

	obsList, err := repo.Observations(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatalf("Observations: %v", err)
	}
	if len(obsList) != 1 {
		t.Fatalf("Observations = %d, want 1", len(obsList))
	}
	if obsList[0].Author != "tutor-a" || obsList[0].Notes != "助詞の復習が必要" {
		t.Fatalf("obsList[0] = %+v, want Author/Notes matching %+v", obsList[0], obs)
	}
	if len(obsList[0].Subjects) != 1 || obsList[0].Subjects[0] != "i-adjective-past" {
		t.Fatalf("Subjects = %+v, want [i-adjective-past]", obsList[0].Subjects)
	}
	if !obsList[0].CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", obsList[0].CreatedAt, now)
	}
}

// TestLessonCompleteWithObservationCrossIdentityIsNoOp pins the
// atomic identity-scoping contract: a call against another identity's
// lesson writes NEITHER the status change NOR the observation.
func TestLessonCompleteWithObservationCrossIdentityIsNoOp(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	lesson := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatal(err)
	}

	obs := storage.LessonObservation{
		ID:        uuid.NewString(),
		LessonID:  lesson.ID,
		Author:    "someone-else-tutor",
		Notes:     "should not attach",
		Subjects:  []string{},
		CreatedAt: time.Now().UTC(),
	}
	_, err := repo.CompleteWithObservation(ctx, learner.IdentityID("someone-else"), lesson.ID, obs, time.Now().UTC())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity CompleteWithObservation err = %v, want storage.ErrNotFound", err)
	}

	// Neither write took effect: status is still "prepared" ...
	still, err := repo.Get(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatal(err)
	}
	if still.Status != "prepared" {
		t.Fatalf("Status = %q, want still prepared (cross-identity call must not flip it)", still.Status)
	}
	if !still.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt = %v, want zero", still.CompletedAt)
	}
	// ... and no observation attached either.
	obsList, err := repo.Observations(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obsList) != 0 {
		t.Fatalf("Observations = %+v, want empty (the cross-identity call must not have attached anything)", obsList)
	}
}

// TestLessonCompleteWithObservationUnknownLessonMisses pins the plain
// not-found case for a lesson ID that doesn't exist at all.
func TestLessonCompleteWithObservationUnknownLessonMisses(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	unknownID := uuid.NewString()
	obs := storage.LessonObservation{
		ID:        uuid.NewString(),
		LessonID:  unknownID,
		Author:    "tutor-a",
		Notes:     "n",
		Subjects:  []string{},
		CreatedAt: time.Now().UTC(),
	}
	_, err := repo.CompleteWithObservation(context.Background(), identity, unknownID, obs, time.Now().UTC())
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestLessonCompleteWithObservationRollsBackBothWritesOnFailure is the
// controller-mandated atomicity proof (code review finding 1): forces
// the transaction's SECOND statement (the observation insert) to fail
// with a genuine constraint violation — a duplicate lesson_observations
// primary key, reusing the exact "reuse an ID that already exists" the
// controller's earlier Anki/feedback rollback tests used
// (TestFeedbackInsertFeedbackFailureRollsBackAlreadyPersistedConcepts)
// — and proves the FIRST statement (CompleteLesson's status flip, which
// already succeeded within this same, still-uncommitted transaction)
// rolled back too: the target lesson is left exactly as it was before
// the call, "prepared" with no CompletedAt and no attached observation.
func TestLessonCompleteWithObservationRollsBackBothWritesOnFailure(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()

	target := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, target); err != nil {
		t.Fatal(err)
	}
	// A second, unrelated lesson that already owns one observation row
	// — its ID is what the failing call below will collide with.
	other := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, other); err != nil {
		t.Fatal(err)
	}
	dupID := uuid.NewString()
	existingObs := storage.LessonObservation{
		ID:        dupID,
		LessonID:  other.ID,
		Author:    "tutor-b",
		Notes:     "pre-existing",
		Subjects:  []string{},
		CreatedAt: time.Now().UTC(),
	}
	if _, err := repo.CompleteWithObservation(ctx, identity, other.ID, existingObs, time.Now().UTC()); err != nil {
		t.Fatalf("seeding the pre-existing observation: %v", err)
	}

	// Now attempt to complete target with an observation that reuses
	// dupID — lesson_observations.id is a primary key, so this INSERT
	// fails with a PK violation AFTER CompleteLesson has already
	// updated target's status within this same, still-uncommitted
	// transaction.
	colliding := storage.LessonObservation{
		ID:        dupID,
		LessonID:  target.ID,
		Author:    "tutor-a",
		Notes:     "should never be visible",
		Subjects:  []string{"i-adjective-past"},
		CreatedAt: time.Now().UTC(),
	}
	if _, err := repo.CompleteWithObservation(ctx, identity, target.ID, colliding, time.Now().UTC()); err == nil {
		t.Fatal("expected an error from the duplicate observation ID PK violation, got nil")
	}

	got, err := repo.Get(ctx, identity, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "prepared" {
		t.Fatalf("Status = %q, want still prepared (CompleteLesson's flip must have rolled back with the failed insert)", got.Status)
	}
	if !got.CompletedAt.IsZero() {
		t.Fatalf("CompletedAt = %v, want zero", got.CompletedAt)
	}

	obsList, err := repo.Observations(ctx, identity, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(obsList) != 0 {
		t.Fatalf("Observations(target) = %+v, want empty (the colliding insert must not have attached)", obsList)
	}
}
