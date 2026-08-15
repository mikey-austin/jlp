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

// TestLessonCompleteSetsStatusAndCompletedAt pins Complete's status
// transition.
func TestLessonCompleteSetsStatusAndCompletedAt(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	lesson := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatal(err)
	}

	got, err := repo.Complete(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if got.Status != "completed" {
		t.Fatalf("Status = %q, want completed", got.Status)
	}
	if got.CompletedAt.IsZero() {
		t.Fatal("CompletedAt is zero, want it set")
	}
}

// TestLessonCompleteCrossIdentityMisses pins Complete's identity scope.
func TestLessonCompleteCrossIdentityMisses(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	ctx := context.Background()
	lesson := testLesson(identity, time.Now().UTC())
	if err := repo.Insert(ctx, lesson); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.Complete(ctx, learner.IdentityID("someone-else"), lesson.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Complete err = %v, want storage.ErrNotFound", err)
	}
}

// TestLessonAddObservationThenListRoundTrips pins AddObservation/
// Observations' full-field round trip, including Subjects surviving
// the jsonb round trip.
func TestLessonAddObservationThenListRoundTrips(t *testing.T) {
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
	if err := repo.AddObservation(ctx, identity, obs); err != nil {
		t.Fatalf("AddObservation: %v", err)
	}

	got, err := repo.Observations(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatalf("Observations: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Observations = %d, want 1", len(got))
	}
	if got[0].Author != "tutor-a" || got[0].Notes != "助詞の復習が必要" {
		t.Fatalf("got[0] = %+v, want Author/Notes matching %+v", got[0], obs)
	}
	if len(got[0].Subjects) != 1 || got[0].Subjects[0] != "i-adjective-past" {
		t.Fatalf("Subjects = %+v, want [i-adjective-past]", got[0].Subjects)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", got[0].CreatedAt, now)
	}
}

// TestLessonAddObservationCrossIdentityMisses pins the "identity check
// via a join to lessons" contract: an observation aimed at another
// identity's lesson is rejected, not silently attached.
func TestLessonAddObservationCrossIdentityMisses(t *testing.T) {
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
	err := repo.AddObservation(ctx, learner.IdentityID("someone-else"), obs)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity AddObservation err = %v, want storage.ErrNotFound", err)
	}

	got, err := repo.Observations(ctx, identity, lesson.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("Observations = %+v, want empty (the cross-identity insert must not have attached)", got)
	}
}

// TestLessonAddObservationUnknownLessonMisses pins the plain
// not-found case for an observation aimed at a lesson ID that doesn't
// exist at all.
func TestLessonAddObservationUnknownLessonMisses(t *testing.T) {
	repo, identity := lessonTestSetup(t)
	obs := storage.LessonObservation{
		ID:        uuid.NewString(),
		LessonID:  uuid.NewString(),
		Author:    "tutor-a",
		Notes:     "n",
		Subjects:  []string{},
		CreatedAt: time.Now().UTC(),
	}
	err := repo.AddObservation(context.Background(), identity, obs)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}
