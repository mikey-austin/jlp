//go:build integration

package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestAIRequestInsertAndListScopedByIdentity(t *testing.T) {
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

	// FK chain: identities -> sessions -> ai_requests.
	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-air-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-air-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	sessions := NewSessionRepository(pool)
	now := time.Now().UTC().Truncate(time.Microsecond)
	sess := session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identityA.ID,
		Title:      "旅行について書く",
		Purpose:    "Diary",
		CreatedAt:  now,
		UpdatedAt:  now,
	}
	if err := sessions.Create(ctx, sess); err != nil {
		t.Fatal(err)
	}

	reqs := NewAIRequestRepository(pool)

	// A session-scoped, successful request and a session-less, failed
	// one, both for identity A.
	scoped := storage.AIRequestRecord{
		ID:            uuid.NewString(),
		Capability:    "structured-generation",
		Provider:      "fake",
		Model:         "fake-1",
		PromptName:    "teacher.feedback",
		PromptVersion: "v1",
		IdentityID:    identityA.ID,
		SessionID:     &sess.ID,
		LatencyMS:     42,
		InputTokens:   1000,
		OutputTokens:  500,
		CostUSD:       0.0105,
		Success:       true,
		CreatedAt:     now,
	}
	if err := reqs.Insert(ctx, scoped); err != nil {
		t.Fatal(err)
	}
	failed := storage.AIRequestRecord{
		ID:            uuid.NewString(),
		Capability:    "structured-generation",
		Provider:      "anthropic",
		Model:         "claude-sonnet-5",
		PromptName:    "teacher.feedback",
		PromptVersion: "v1",
		IdentityID:    identityA.ID,
		LatencyMS:     10,
		Success:       false,
		Error:         "provider timeout",
		CreatedAt:     now.Add(time.Second),
	}
	if err := reqs.Insert(ctx, failed); err != nil {
		t.Fatal(err)
	}

	// A request for identity B must never show up in identity A's list.
	other := storage.AIRequestRecord{
		ID:         uuid.NewString(),
		Provider:   "fake",
		Model:      "fake-1",
		IdentityID: identityB.ID,
		Success:    true,
		CreatedAt:  now,
	}
	if err := reqs.Insert(ctx, other); err != nil {
		t.Fatal(err)
	}

	list, err := reqs.List(ctx, identityA.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Fatalf("List returned %d records, want 2", len(list))
	}
	// Newest first.
	if list[0].ID != failed.ID || list[1].ID != scoped.ID {
		t.Fatalf("List order = [%s,%s], want newest-first [%s,%s]", list[0].ID, list[1].ID, failed.ID, scoped.ID)
	}

	got := list[1] // the scoped, successful record
	if got.SessionID == nil || *got.SessionID != sess.ID {
		t.Fatalf("scoped record SessionID = %v, want %s", got.SessionID, sess.ID)
	}
	if !got.Success {
		t.Error("scoped record Success = false, want true")
	}
	if got.InputTokens != 1000 || got.OutputTokens != 500 {
		t.Errorf("scoped record tokens = %d/%d, want 1000/500", got.InputTokens, got.OutputTokens)
	}
	if diff := got.CostUSD - 0.0105; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("scoped record CostUSD = %v, want 0.0105", got.CostUSD)
	}
	if got.Provider != "fake" || got.Model != "fake-1" {
		t.Errorf("scoped record Provider/Model = %q/%q, want fake/fake-1", got.Provider, got.Model)
	}

	failedGot := list[0]
	if failedGot.SessionID != nil {
		t.Errorf("failed record SessionID = %v, want nil", failedGot.SessionID)
	}
	if failedGot.Success {
		t.Error("failed record Success = true, want false")
	}
	if failedGot.Error != "provider timeout" {
		t.Errorf("failed record Error = %q, want %q", failedGot.Error, "provider timeout")
	}
	if failedGot.CostUSD != 0 {
		t.Errorf("failed record CostUSD = %v, want 0", failedGot.CostUSD)
	}

	// limit is honored.
	limited, err := reqs.List(ctx, identityA.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("List limit=1 returned %d rows, want 1", len(limited))
	}
}
