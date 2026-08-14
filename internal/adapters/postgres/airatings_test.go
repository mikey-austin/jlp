//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// seedAIRequest inserts a minimal ai_requests row for identity so a
// rating has a real FK target — ai_ratings.ai_request_id references
// ai_requests(id).
func seedAIRequest(t *testing.T, ctx context.Context, reqs *AIRequestRepository, identity learner.IdentityID) string {
	t.Helper()
	id := uuid.NewString()
	rec := storage.AIRequestRecord{
		ID:         id,
		Provider:   "fake",
		Model:      "fake-1",
		IdentityID: identity,
		Success:    true,
		CreatedAt:  time.Now().UTC(),
	}
	if err := reqs.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestAIRatingUpsertReplacesPreviousRating: rating the same request
// twice (3, then 5) must leave exactly one rating in place — the
// second value, not a second row — since AIRatingRepository.Upsert
// documents "one rating per (ai_request_id, identity)".
func TestAIRatingUpsertReplacesPreviousRating(t *testing.T) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-air-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	requestID := seedAIRequest(t, ctx, reqs, identity.ID)

	ratings := NewAIRatingRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestID,
		IdentityID:  identity.ID,
		Rating:      3,
		CreatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestID,
		IdentityID:  identity.ID,
		Rating:      5,
		CreatedAt:   now.Add(time.Second),
	}); err != nil {
		t.Fatal(err)
	}

	got, err := ratings.ForRequests(ctx, identity.ID, []string{requestID})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForRequests returned %d entries, want 1 (upsert must replace, not duplicate): %v", len(got), got)
	}
	if got[requestID] != 5 {
		t.Fatalf("ForRequests[%s] = %d, want 5 (second upsert must win)", requestID, got[requestID])
	}
}

// TestAIRatingForRequestsIsIdentityScoped: identity B's rating of its
// own request must never appear in identity A's ForRequests result,
// even when A explicitly asks about B's request id — the same
// cross-identity isolation every other identity-scoped repository in
// this package enforces.
func TestAIRatingForRequestsIsIdentityScoped(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-air-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-air-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	requestA := seedAIRequest(t, ctx, reqs, identityA.ID)
	requestB := seedAIRequest(t, ctx, reqs, identityB.ID)

	ratings := NewAIRatingRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	if err := ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestA,
		IdentityID:  identityA.ID,
		Rating:      2,
		CreatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestB,
		IdentityID:  identityB.ID,
		Rating:      5,
		CreatedAt:   now,
	}); err != nil {
		t.Fatal(err)
	}

	// A asks about both request ids; only A's own rating of requestA
	// must come back.
	got, err := ratings.ForRequests(ctx, identityA.ID, []string{requestA, requestB})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("ForRequests(A, [A,B]) returned %d entries, want 1 (B's rating must not leak): %v", len(got), got)
	}
	if got[requestA] != 2 {
		t.Fatalf("ForRequests(A, ...)[requestA] = %d, want 2", got[requestA])
	}
	if _, ok := got[requestB]; ok {
		t.Fatalf("ForRequests(A, ...) leaked identity B's rating of requestB: %v", got)
	}
}

// TestAIRatingUpsertRejectsRequestOwnedByAnotherIdentity: Upsert must
// verify ai_request_id actually belongs to the identity doing the
// rating, not just that some ai_requests row with that id exists — an
// identity should never be able to attach a rating to another
// identity's AI request by supplying its id (observed elsewhere, e.g.
// shared logs). A caller who tries gets storage.ErrNotFound, the same
// "not yours" signal every other identity-scoped write in this package
// gives, and nothing is written.
func TestAIRatingUpsertRejectsRequestOwnedByAnotherIdentity(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-air-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-air-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	requestB := seedAIRequest(t, ctx, reqs, identityB.ID)

	ratings := NewAIRatingRepository(pool)
	err = ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestB,
		IdentityID:  identityA.ID,
		Rating:      5,
		CreatedAt:   time.Now().UTC(),
	})
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("Upsert(A rating B's request) err = %v, want storage.ErrNotFound", err)
	}

	got, err := ratings.ForRequests(ctx, identityA.ID, []string{requestB})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("forged rating was written despite rejection: %v", got)
	}
}
