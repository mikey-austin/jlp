//go:build integration

package postgres

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// closeEnough compares floats with a small tolerance — aggregate
// arithmetic (division, AVG) round-trips through float64 in the
// adapter, so exact equality would be brittle.
func closeEnough(got, want, tol float64) bool {
	return math.Abs(got-want) <= tol
}

// insertAIRequestFull inserts a fully-specified ai_requests row — unlike
// airatings_test.go's seedAIRequest, this test needs to control
// provider/model/prompt/latency/cost/success independently per row to
// pin the aggregate math (avg, p95, success rate, cost sum).
func insertAIRequestFull(t *testing.T, ctx context.Context, reqs *AIRequestRepository, identity learner.IdentityID, provider, model, promptName, promptVersion string, latencyMS int, costUSD float64, success bool, createdAt time.Time) string {
	t.Helper()
	id := uuid.NewString()
	rec := storage.AIRequestRecord{
		ID:            id,
		Provider:      provider,
		Model:         model,
		PromptName:    promptName,
		PromptVersion: promptVersion,
		IdentityID:    identity,
		LatencyMS:     latencyMS,
		CostUSD:       costUSD,
		Success:       success,
		CreatedAt:     createdAt,
	}
	if err := reqs.Insert(ctx, rec); err != nil {
		t.Fatal(err)
	}
	return id
}

func rateRequest(t *testing.T, ctx context.Context, ratings *AIRatingRepository, requestID string, identity learner.IdentityID, rating int) {
	t.Helper()
	if err := ratings.Upsert(ctx, storage.AIRating{
		ID:          uuid.NewString(),
		AIRequestID: requestID,
		IdentityID:  identity,
		Rating:      rating,
		CreatedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// TestAIQualityByProviderAggregatesRequestsRatingsLatencyAndCost seeds
// five "fake"/"fake-1" requests (four success, one failure) with known
// latencies — 10, 20, 30, 40, 1000ms — and ratings on three of them, plus
// one "anthropic"/"claude-sonnet-5" request, all for one identity.
// ByProvider must return two rows, sorted by provider then model, with
// the exact aggregate numbers: request count, success rate (4/5=0.8),
// average rating over rated requests only ((3+5+4)/3), average latency
// (220ms), p95 latency via percentile_cont(0.95) — with n=5 sorted
// latencies [10,20,30,40,1000], rank=0.95*4=3.8, interpolating between
// index 3 (40) and 4 (1000) gives 40+0.8*(1000-40)=808 — and total cost.
func TestAIQualityByProviderAggregatesRequestsRatingsLatencyAndCost(t *testing.T) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-aiq-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	ratings := NewAIRatingRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	latencies := []int{10, 20, 30, 40, 1000}
	successes := []bool{true, true, true, true, false}
	costs := []float64{0.001, 0.002, 0.003, 0.004, 0.005}
	ratingVals := []int{3, 0, 5, 4, 0} // 0 = leave unrated
	for i := range latencies {
		id := insertAIRequestFull(t, ctx, reqs, identity.ID, "fake", "fake-1", "teacher.feedback", "v1", latencies[i], costs[i], successes[i], now.Add(time.Duration(i)*time.Second))
		if ratingVals[i] != 0 {
			rateRequest(t, ctx, ratings, id, identity.ID, ratingVals[i])
		}
	}
	insertAIRequestFull(t, ctx, reqs, identity.ID, "anthropic", "claude-sonnet-5", "teacher.feedback", "v2", 50, 0.02, true, now.Add(10*time.Second))

	quality := NewAIQualityRepository(pool)
	stats, err := quality.ByProvider(ctx, identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("ByProvider returned %d rows, want 2: %+v", len(stats), stats)
	}
	// Sorted by provider name: "anthropic" < "fake".
	anthropic, fake := stats[0], stats[1]

	if anthropic.Provider != "anthropic" || anthropic.Model != "claude-sonnet-5" {
		t.Fatalf("stats[0] = %+v, want anthropic/claude-sonnet-5", anthropic)
	}
	if anthropic.Requests != 1 {
		t.Errorf("anthropic Requests = %d, want 1", anthropic.Requests)
	}

	if fake.Provider != "fake" || fake.Model != "fake-1" {
		t.Fatalf("stats[1] = %+v, want fake/fake-1", fake)
	}
	if fake.Requests != 5 {
		t.Errorf("fake Requests = %d, want 5", fake.Requests)
	}
	if !closeEnough(fake.SuccessRate, 0.8, 1e-9) {
		t.Errorf("fake SuccessRate = %v, want 0.8", fake.SuccessRate)
	}
	wantAvgRating := (3.0 + 5.0 + 4.0) / 3.0
	if !closeEnough(fake.AvgRating, wantAvgRating, 1e-9) {
		t.Errorf("fake AvgRating = %v, want %v", fake.AvgRating, wantAvgRating)
	}
	if fake.AvgLatencyMS != 220 {
		t.Errorf("fake AvgLatencyMS = %d, want 220", fake.AvgLatencyMS)
	}
	if fake.P95LatencyMS != 808 {
		t.Errorf("fake P95LatencyMS = %d, want 808", fake.P95LatencyMS)
	}
	wantCost := 0.001 + 0.002 + 0.003 + 0.004 + 0.005
	if !closeEnough(fake.TotalCostUSD, wantCost, 1e-9) {
		t.Errorf("fake TotalCostUSD = %v, want %v", fake.TotalCostUSD, wantCost)
	}
}

// TestAIQualityByPromptDistinguishesPromptVersions seeds
// teacher.feedback v1, teacher.feedback v2, and drill.generate v1 rows
// with distinct ratings so ByPrompt must keep each (name, version) pair
// as its own row rather than folding versions of the same prompt name
// together (PRD §26's whole point: seeing whether a prompt rewrite
// helped).
func TestAIQualityByPromptDistinguishesPromptVersions(t *testing.T) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-aiq-" + uuid.NewString()), DisplayName: "A"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	ratings := NewAIRatingRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	v1a := insertAIRequestFull(t, ctx, reqs, identity.ID, "fake", "fake-1", "teacher.feedback", "v1", 10, 0, true, now)
	v1b := insertAIRequestFull(t, ctx, reqs, identity.ID, "fake", "fake-1", "teacher.feedback", "v1", 10, 0, false, now.Add(time.Second))
	v2a := insertAIRequestFull(t, ctx, reqs, identity.ID, "fake", "fake-1", "teacher.feedback", "v2", 10, 0, true, now.Add(2*time.Second))
	drillA := insertAIRequestFull(t, ctx, reqs, identity.ID, "fake", "fake-1", "drill.generate", "v1", 10, 0, true, now.Add(3*time.Second))

	rateRequest(t, ctx, ratings, v1a, identity.ID, 2)
	rateRequest(t, ctx, ratings, v1b, identity.ID, 4)
	rateRequest(t, ctx, ratings, v2a, identity.ID, 5)
	rateRequest(t, ctx, ratings, drillA, identity.ID, 3)

	quality := NewAIQualityRepository(pool)
	stats, err := quality.ByPrompt(ctx, identity.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 3 {
		t.Fatalf("ByPrompt returned %d rows, want 3 (drill.generate/v1, teacher.feedback/v1, teacher.feedback/v2): %+v", len(stats), stats)
	}
	// Sorted by prompt_name then prompt_version: drill.generate < teacher.feedback.
	drill, v1, v2 := stats[0], stats[1], stats[2]

	if drill.PromptName != "drill.generate" || drill.PromptVersion != "v1" {
		t.Fatalf("stats[0] = %+v, want drill.generate/v1", drill)
	}
	if drill.Requests != 1 || !closeEnough(drill.AvgRating, 3.0, 1e-9) {
		t.Errorf("drill.generate/v1 = %+v, want Requests=1 AvgRating=3", drill)
	}

	if v1.PromptName != "teacher.feedback" || v1.PromptVersion != "v1" {
		t.Fatalf("stats[1] = %+v, want teacher.feedback/v1", v1)
	}
	if v1.Requests != 2 {
		t.Errorf("teacher.feedback/v1 Requests = %d, want 2", v1.Requests)
	}
	if !closeEnough(v1.AvgRating, 3.0, 1e-9) { // (2+4)/2
		t.Errorf("teacher.feedback/v1 AvgRating = %v, want 3", v1.AvgRating)
	}
	if !closeEnough(v1.SuccessRate, 0.5, 1e-9) { // 1 success, 1 failure
		t.Errorf("teacher.feedback/v1 SuccessRate = %v, want 0.5", v1.SuccessRate)
	}

	if v2.PromptName != "teacher.feedback" || v2.PromptVersion != "v2" {
		t.Fatalf("stats[2] = %+v, want teacher.feedback/v2", v2)
	}
	if v2.Requests != 1 || !closeEnough(v2.AvgRating, 5.0, 1e-9) || !closeEnough(v2.SuccessRate, 1.0, 1e-9) {
		t.Errorf("teacher.feedback/v2 = %+v, want Requests=1 AvgRating=5 SuccessRate=1", v2)
	}
}

// TestAIQualityIsolatedByIdentity: identity B's requests and ratings
// must never leak into identity A's ByProvider/ByPrompt results, even
// when B rates its own requests highly and A's requests are unrated —
// the same cross-identity isolation every other identity-scoped
// repository in this package enforces (see airequests_test.go,
// airatings_test.go).
func TestAIQualityIsolatedByIdentity(t *testing.T) {
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
	identityA := learner.Identity{ID: learner.IdentityID("test-aiq-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-aiq-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)
	ratings := NewAIRatingRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)

	insertAIRequestFull(t, ctx, reqs, identityA.ID, "fake", "fake-1", "teacher.feedback", "v1", 10, 0.001, true, now)
	reqB := insertAIRequestFull(t, ctx, reqs, identityB.ID, "fake", "fake-1", "teacher.feedback", "v1", 999, 9.0, true, now)
	rateRequest(t, ctx, ratings, reqB, identityB.ID, 5)

	quality := NewAIQualityRepository(pool)

	byProviderA, err := quality.ByProvider(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byProviderA) != 1 {
		t.Fatalf("ByProvider(A) returned %d rows, want 1 (B's request must not leak): %+v", len(byProviderA), byProviderA)
	}
	if byProviderA[0].Requests != 1 {
		t.Errorf("ByProvider(A)[0].Requests = %d, want 1", byProviderA[0].Requests)
	}
	if byProviderA[0].AvgRating != 0 {
		t.Errorf("ByProvider(A)[0].AvgRating = %v, want 0 (A's request is unrated; B's rating of B's own request must not leak in)", byProviderA[0].AvgRating)
	}
	if byProviderA[0].AvgLatencyMS != 10 {
		t.Errorf("ByProvider(A)[0].AvgLatencyMS = %d, want 10 (B's 999ms request must not leak in)", byProviderA[0].AvgLatencyMS)
	}

	byPromptA, err := quality.ByPrompt(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(byPromptA) != 1 || byPromptA[0].Requests != 1 || byPromptA[0].AvgRating != 0 {
		t.Fatalf("ByPrompt(A) = %+v, want exactly A's own unrated request", byPromptA)
	}
}
