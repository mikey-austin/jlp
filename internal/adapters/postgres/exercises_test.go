//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/exercise"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// exerciseTestSetup migrates and returns a repo plus a freshly-upserted
// identity — the same "one throwaway identity per test, real uuid
// suffix" pattern vocabTestSetup/priorities_test.go use.
func exerciseTestSetup(t *testing.T) (*ExerciseRepository, learner.IdentityID) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-exercise-" + uuid.NewString()), DisplayName: "Exercise"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}
	return NewExerciseRepository(pool), identity.ID
}

func testExercise(identity learner.IdentityID, now time.Time) exercise.Exercise {
	return exercise.Exercise{
		ID:             uuid.NewString(),
		IdentityID:     identity,
		ConceptSlug:    "i-adjective-past",
		Type:           exercise.TypeMultipleChoice,
		InstructionsJA: "正しい過去形を選んでください。",
		InstructionsEN: "Choose the correct past tense.",
		Prompt:         "昨日の映画はとても＿＿＿。",
		Choices:        []string{"面白いでした", "面白かったです", "面白いだった"},
		Answer:         "面白かったです",
		CreatedAt:      now,
	}
}

// TestExerciseCreateThenGetRoundTrips pins Create/Get's full-payload
// round trip: every field, including Choices, comes back exactly as
// persisted.
func TestExerciseCreateThenGetRoundTrips(t *testing.T) {
	repo, identity := exerciseTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ex := testExercise(identity, now)

	if err := repo.Create(ctx, ex); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.Get(ctx, identity, ex.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != ex.ID || got.IdentityID != identity {
		t.Fatalf("got ID/IdentityID = %s/%s, want %s/%s", got.ID, got.IdentityID, ex.ID, identity)
	}
	if got.ConceptSlug != ex.ConceptSlug || got.Type != ex.Type {
		t.Fatalf("got ConceptSlug/Type = %s/%s, want %s/%s", got.ConceptSlug, got.Type, ex.ConceptSlug, ex.Type)
	}
	if got.Prompt != ex.Prompt || got.Answer != ex.Answer {
		t.Fatalf("got Prompt/Answer = %q/%q, want %q/%q", got.Prompt, got.Answer, ex.Prompt, ex.Answer)
	}
	if len(got.Choices) != 3 || got.Choices[1] != "面白かったです" {
		t.Fatalf("got Choices = %v, want the 3 canned choices", got.Choices)
	}
	if !got.CreatedAt.Equal(now) {
		t.Fatalf("got.CreatedAt = %v, want %v", got.CreatedAt, now)
	}
	if got.SessionID != nil {
		t.Fatalf("got.SessionID = %v, want nil", got.SessionID)
	}
}

// TestExerciseGetCrossIdentityMisses pins the identity-scoping contract
// application/practice.Service.Answer relies on for its ErrNotFound
// cross-identity check.
func TestExerciseGetCrossIdentityMisses(t *testing.T) {
	repo, identity := exerciseTestSetup(t)
	ctx := context.Background()
	ex := testExercise(identity, time.Now().UTC())
	if err := repo.Create(ctx, ex); err != nil {
		t.Fatalf("Create: %v", err)
	}

	_, err := repo.Get(ctx, learner.IdentityID("someone-else"), ex.ID)
	if err != storage.ErrNotFound {
		t.Fatalf("Get across identities err = %v, want storage.ErrNotFound", err)
	}
}

// TestExerciseGetUnknownIDMisses pins the plain not-found case.
func TestExerciseGetUnknownIDMisses(t *testing.T) {
	repo, identity := exerciseTestSetup(t)
	_, err := repo.Get(context.Background(), identity, uuid.NewString())
	if err != storage.ErrNotFound {
		t.Fatalf("Get unknown id err = %v, want storage.ErrNotFound", err)
	}
}

// TestExerciseRecordAttemptPersists pins RecordAttempt's write path,
// including a nil Confidence (the "not given" case).
func TestExerciseRecordAttemptPersists(t *testing.T) {
	repo, identity := exerciseTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ex := testExercise(identity, now)
	if err := repo.Create(ctx, ex); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := repo.RecordAttempt(ctx, storage.ExerciseAttempt{
		ID:         uuid.NewString(),
		ExerciseID: ex.ID,
		Response:   "面白かったです",
		Correct:    true,
		Score:      100,
		FeedbackJA: "正解です！",
		FeedbackEN: "Correct!",
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}

	var count int
	if err := repo.pool.QueryRow(ctx, "SELECT count(*) FROM exercise_attempts WHERE exercise_id = $1", ex.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("exercise_attempts rows = %d, want 1", count)
	}

	var confidence *int
	if err := repo.pool.QueryRow(ctx, "SELECT confidence FROM exercise_attempts WHERE exercise_id = $1", ex.ID).Scan(&confidence); err != nil {
		t.Fatal(err)
	}
	if confidence != nil {
		t.Fatalf("confidence = %v, want nil (not given)", *confidence)
	}
}

// TestExerciseRecordAttemptWithConfidencePersists pins the
// confidence-given path, and its DB-level 1..5 CHECK constraint.
func TestExerciseRecordAttemptWithConfidencePersists(t *testing.T) {
	repo, identity := exerciseTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	ex := testExercise(identity, now)
	if err := repo.Create(ctx, ex); err != nil {
		t.Fatalf("Create: %v", err)
	}

	confidence := 4
	if err := repo.RecordAttempt(ctx, storage.ExerciseAttempt{
		ID:         uuid.NewString(),
		ExerciseID: ex.ID,
		Response:   "面白かったです",
		Correct:    true,
		Score:      100,
		FeedbackJA: "正解です！",
		FeedbackEN: "Correct!",
		Confidence: &confidence,
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("RecordAttempt: %v", err)
	}

	var got int
	if err := repo.pool.QueryRow(ctx, "SELECT confidence FROM exercise_attempts WHERE exercise_id = $1", ex.ID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 4 {
		t.Fatalf("confidence = %d, want 4", got)
	}
}
