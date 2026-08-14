package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeAIRatingRepo is an in-memory storage.AIRatingRepository double for
// HTTP-layer tests: Upsert records the last call it received (so a test
// can assert exactly what the handler sent), and also folds the value
// into ratings so ForRequests can answer realistically for the /ai page
// tests — mirroring how the real postgres upsert-then-read round trips.
type fakeAIRatingRepo struct {
	lastUpsert *storage.AIRating
	ratings    map[string]int // AIRequestID -> Rating
	err        error
}

func newFakeAIRatingRepo() *fakeAIRatingRepo {
	return &fakeAIRatingRepo{ratings: map[string]int{}}
}

func (f *fakeAIRatingRepo) Upsert(_ context.Context, r storage.AIRating) error {
	if f.err != nil {
		return f.err
	}
	cp := r
	f.lastUpsert = &cp
	f.ratings[r.AIRequestID] = r.Rating
	return nil
}

func (f *fakeAIRatingRepo) ForRequests(_ context.Context, _ learner.IdentityID, requestIDs []string) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := map[string]int{}
	for _, id := range requestIDs {
		if v, ok := f.ratings[id]; ok {
			out[id] = v
		}
	}
	return out, nil
}

// fakeAIRequestRepo is an in-memory storage.AIRequestRepository double:
// List always returns whatever records a test configures for the /ai
// page, unconditionally (the postgres repo's identity scoping is
// covered by its own integration test — see airequests_test.go).
type fakeAIRequestRepo struct {
	records []storage.AIRequestRecord
}

func (f fakeAIRequestRepo) Insert(context.Context, storage.AIRequestRecord) error { return nil }

func (f fakeAIRequestRepo) List(context.Context, learner.IdentityID, int) ([]storage.AIRequestRecord, error) {
	return f.records, nil
}

func aiTestOptions() Options {
	opts := testOptions()
	opts.AIRatings = newFakeAIRatingRepo()
	return opts
}

// TestRatingsCreateReturnsNoContentAndCapturesRating: POSTing a valid
// 1..5 rating returns 204 with no body, and the repository receives
// exactly the request/rating pair the form carried.
func TestRatingsCreateReturnsNoContentAndCapturesRating(t *testing.T) {
	opts := aiTestOptions()
	repo := opts.AIRatings.(*fakeAIRatingRepo)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("ai_request_id", "11111111-1111-1111-1111-111111111111")
	form.Set("rating", "4")
	rec := postForm(t, srv.HandlerForTest(), "/ratings", form)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("POST /ratings status = %d, want 204, body=%s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("POST /ratings body = %q, want empty", rec.Body.String())
	}
	if repo.lastUpsert == nil {
		t.Fatal("repository did not receive an Upsert call")
	}
	if repo.lastUpsert.AIRequestID != "11111111-1111-1111-1111-111111111111" {
		t.Errorf("captured AIRequestID = %q, want %q", repo.lastUpsert.AIRequestID, "11111111-1111-1111-1111-111111111111")
	}
	if repo.lastUpsert.Rating != 4 {
		t.Errorf("captured Rating = %d, want 4", repo.lastUpsert.Rating)
	}
	if repo.lastUpsert.IdentityID != "dev" {
		t.Errorf("captured IdentityID = %q, want %q (testOptions authenticates as dev)", repo.lastUpsert.IdentityID, "dev")
	}
}

// TestRatingsCreateOutOfRangeReturnsBadRequest: a rating outside 1..5
// must be rejected before it ever reaches the repository — the DB CHECK
// constraint is a backstop, not the first line of validation.
func TestRatingsCreateOutOfRangeReturnsBadRequest(t *testing.T) {
	opts := aiTestOptions()
	repo := opts.AIRatings.(*fakeAIRatingRepo)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("ai_request_id", "11111111-1111-1111-1111-111111111111")
	form.Set("rating", "9")
	rec := postForm(t, srv.HandlerForTest(), "/ratings", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /ratings rating=9 status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastUpsert != nil {
		t.Error("repository received an Upsert call for an out-of-range rating")
	}
}

// TestRatingsCreateNonNumericReturnsBadRequest: a non-integer rating
// field is a client error, not a 500 — same shape as the feedback
// handler's bad-start test.
func TestRatingsCreateNonNumericReturnsBadRequest(t *testing.T) {
	opts := aiTestOptions()
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("ai_request_id", "11111111-1111-1111-1111-111111111111")
	form.Set("rating", "not-a-number")
	rec := postForm(t, srv.HandlerForTest(), "/ratings", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /ratings rating=not-a-number status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestRatingsCreateMalformedAIRequestIDReturnsBadRequest: a missing or
// non-UUID ai_request_id is a client error caught before it ever
// reaches the repository, not a 500 from a failed parse deep inside
// the postgres adapter.
func TestRatingsCreateMalformedAIRequestIDReturnsBadRequest(t *testing.T) {
	opts := aiTestOptions()
	repo := opts.AIRatings.(*fakeAIRatingRepo)
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("ai_request_id", "not-a-uuid")
	form.Set("rating", "4")
	rec := postForm(t, srv.HandlerForTest(), "/ratings", form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST /ratings ai_request_id=not-a-uuid status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if repo.lastUpsert != nil {
		t.Error("repository received an Upsert call for a malformed ai_request_id")
	}
}

// TestRatingsCreateForRequestNotOwnedByIdentityReturnsNotFound: when
// the repository reports storage.ErrNotFound — its signal for "no such
// ai_request for this identity" (postgres.AIRatingRepository.Upsert
// verifies ownership via a join) — the handler must surface 404, not a
// 500 or a false 204.
func TestRatingsCreateForRequestNotOwnedByIdentityReturnsNotFound(t *testing.T) {
	opts := aiTestOptions()
	repo := opts.AIRatings.(*fakeAIRatingRepo)
	repo.err = storage.ErrNotFound
	srv := NewServer(opts)

	form := url.Values{}
	form.Set("ai_request_id", "11111111-1111-1111-1111-111111111111")
	form.Set("rating", "4")
	rec := postForm(t, srv.HandlerForTest(), "/ratings", form)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("POST /ratings for unowned request status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAIRequestsPageRendersTable covers GET /ai: the identity's recent
// ai_requests render as a table with provider, model, prompt
// name+version, latency, tokens, cost, success, and — when the identity
// has rated a request — its rating.
func TestAIRequestsPageRendersTable(t *testing.T) {
	opts := aiTestOptions()
	now := time.Now().UTC()
	opts.AIRequests = fakeAIRequestRepo{records: []storage.AIRequestRecord{
		{
			ID:            "22222222-2222-2222-2222-222222222222",
			Provider:      "fake",
			Model:         "fake-1",
			PromptName:    "teacher.feedback",
			PromptVersion: "v1",
			IdentityID:    "dev",
			LatencyMS:     42,
			InputTokens:   100,
			OutputTokens:  50,
			CostUSD:       0,
			Success:       true,
			CreatedAt:     now,
		},
	}}
	ratingRepo := opts.AIRatings.(*fakeAIRatingRepo)
	ratingRepo.ratings["22222222-2222-2222-2222-222222222222"] = 4

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /ai status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"fake", "fake-1", "teacher.feedback", "v1", "42", "100", "50"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /ai body missing %q: %s", want, body)
		}
	}
	if !strings.Contains(body, ">4<") {
		t.Errorf("GET /ai body missing rendered rating 4: %s", body)
	}
}
