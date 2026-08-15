//go:build integration

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// ankiTestSetup migrates and returns a repo plus a freshly-upserted
// identity — the same "one throwaway identity per test" pattern
// exercises_test.go's exerciseTestSetup uses.
func ankiTestSetup(t *testing.T) (*AnkiCardRepository, learner.IdentityID) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-anki-" + uuid.NewString()), DisplayName: "Anki"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}
	return NewAnkiCardRepository(pool), identity.ID
}

func testAnkiCard(identity learner.IdentityID, status string, now time.Time) storage.AnkiCard {
	return storage.AnkiCard{
		ID:         uuid.NewString(),
		IdentityID: identity,
		SourceType: "correction",
		SourceID:   uuid.NewString(),
		Front:      "「とても面白いでした」— 何が不自然？",
		Back:       "「とても面白かったです」\n\n理由: い形容詞の過去形は〜かったを使います。",
		Notes:      "i-adjective-past",
		Status:     status,
		CreatedAt:  now,
	}
}

// TestAnkiInsertThenListRoundTrips pins Insert/List's full-field round
// trip.
func TestAnkiInsertThenListRoundTrips(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	card := testAnkiCard(identity, "draft", now)

	if err := repo.Insert(ctx, card); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List = %d cards, want 1", len(got))
	}
	if got[0].ID != card.ID || got[0].Front != card.Front || got[0].Back != card.Back {
		t.Fatalf("got = %+v, want front/back matching %+v", got[0], card)
	}
	if got[0].Notes != card.Notes {
		t.Fatalf("Notes = %q, want %q", got[0].Notes, card.Notes)
	}
	if got[0].Status != "draft" {
		t.Fatalf("Status = %q, want draft", got[0].Status)
	}
	if !got[0].CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt = %v, want %v", got[0].CreatedAt, now)
	}
}

// TestAnkiListFiltersByStatusEmptyMeansAll pins the status filter
// contract: a specific status narrows, "" returns everything.
func TestAnkiListFiltersByStatusEmptyMeansAll(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	draft := testAnkiCard(identity, "draft", now)
	approved := testAnkiCard(identity, "approved", now)
	if err := repo.Insert(ctx, draft); err != nil {
		t.Fatal(err)
	}
	if err := repo.Insert(ctx, approved); err != nil {
		t.Fatal(err)
	}

	drafts, err := repo.List(ctx, identity, "draft")
	if err != nil {
		t.Fatal(err)
	}
	if len(drafts) != 1 || drafts[0].ID != draft.ID {
		t.Fatalf("List(draft) = %+v, want exactly [%s]", drafts, draft.ID)
	}

	all, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List(\"\") = %d cards, want 2 (both)", len(all))
	}
}

// TestAnkiUpdateStatusCrossIdentityMisses pins the identity-scoping
// contract application/anki.Service.SetStatus relies on for its
// cross-identity ErrNotFound check.
func TestAnkiUpdateStatusCrossIdentityMisses(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	ctx := context.Background()
	card := testAnkiCard(identity, "draft", time.Now().UTC())
	if err := repo.Insert(ctx, card); err != nil {
		t.Fatal(err)
	}

	if _, err := repo.UpdateStatus(ctx, learner.IdentityID("someone-else"), card.ID, "approved"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity UpdateStatus err = %v, want storage.ErrNotFound", err)
	}

	updated, err := repo.UpdateStatus(ctx, identity, card.ID, "approved")
	if err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if updated.Status != "approved" {
		t.Fatalf("Status = %q, want approved", updated.Status)
	}
}

// TestAnkiUpdateStatusUnknownIDMisses pins the plain not-found case.
func TestAnkiUpdateStatusUnknownIDMisses(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	_, err := repo.UpdateStatus(context.Background(), identity, uuid.NewString(), "approved")
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("err = %v, want storage.ErrNotFound", err)
	}
}

// TestAnkiTakeApprovedForExportRoundTrip pins the export pipeline's
// full round trip: only "approved" cards are taken, they come back
// already marked "exported", and a second immediate call sees nothing
// left approved (the "second immediate export is empty" contract
// application/anki.Service.ExportTSV relies on) — all now achieved by
// ONE atomic TakeApprovedForExport call rather than a separate read
// (ApprovedForExport) plus write (MarkExported), per the controller's
// ruling on Phase 3 Task 3's code review (finding 2).
func TestAnkiTakeApprovedForExportRoundTrip(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	approved := testAnkiCard(identity, "draft", now)
	if err := repo.Insert(ctx, approved); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateStatus(ctx, identity, approved.ID, "approved"); err != nil {
		t.Fatal(err)
	}

	draft := testAnkiCard(identity, "draft", now)
	if err := repo.Insert(ctx, draft); err != nil {
		t.Fatal(err)
	}

	rejected := testAnkiCard(identity, "draft", now)
	if err := repo.Insert(ctx, rejected); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateStatus(ctx, identity, rejected.ID, "rejected"); err != nil {
		t.Fatal(err)
	}

	taken, err := repo.TakeApprovedForExport(ctx, identity, time.Now().UTC())
	if err != nil {
		t.Fatalf("TakeApprovedForExport: %v", err)
	}
	if len(taken) != 1 || taken[0].ID != approved.ID {
		t.Fatalf("TakeApprovedForExport = %+v, want exactly [%s] (draft/rejected excluded)", taken, approved.ID)
	}
	if taken[0].Status != "exported" {
		t.Fatalf("taken[0].Status = %q, want exported (already marked, atomically, by the take itself)", taken[0].Status)
	}

	afterTake, err := repo.List(ctx, identity, "exported")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterTake) != 1 || afterTake[0].ID != approved.ID {
		t.Fatalf("List(exported) = %+v, want exactly [%s]", afterTake, approved.ID)
	}

	// Second immediate call: nothing left approved, so it returns empty
	// — a safe no-op, not an error.
	secondRound, err := repo.TakeApprovedForExport(ctx, identity, time.Now().UTC())
	if err != nil {
		t.Fatalf("second TakeApprovedForExport: %v", err)
	}
	if len(secondRound) != 0 {
		t.Fatalf("second TakeApprovedForExport = %+v, want empty", secondRound)
	}
}

// TestAnkiTakeApprovedForExportConcurrentCallsReturnDisjointSets is the
// controller-mandated regression test for finding 2: two goroutines
// calling TakeApprovedForExport concurrently for the SAME identity must
// never both see the same approved row. With N approved cards and two
// concurrent callers, the only correct outcomes are "one caller gets
// all N, the other gets none" — proving the SELECT ... FOR UPDATE row
// lock actually serializes the two transactions rather than letting
// both read the same "approved" snapshot before either commits its
// mark (the exact race the old two-call ApprovedForExport+MarkExported
// pair was vulnerable to).
func TestAnkiTakeApprovedForExportConcurrentCallsReturnDisjointSets(t *testing.T) {
	repo, identity := ankiTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC()

	const cardCount = 5
	want := make(map[string]bool, cardCount)
	for i := 0; i < cardCount; i++ {
		card := testAnkiCard(identity, "draft", now)
		if err := repo.Insert(ctx, card); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.UpdateStatus(ctx, identity, card.ID, "approved"); err != nil {
			t.Fatal(err)
		}
		want[card.ID] = true
	}

	var wg sync.WaitGroup
	results := make([][]storage.AnkiCard, 2)
	errs := make([]error, 2)
	wg.Add(2)
	for i := range 2 {
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = repo.TakeApprovedForExport(ctx, identity, time.Now().UTC())
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: TakeApprovedForExport: %v", i, err)
		}
	}

	total := len(results[0]) + len(results[1])
	if total != cardCount {
		t.Fatalf("total cards taken across both calls = %d, want %d (0+%d or %d+0 — every card taken exactly once)", total, cardCount, cardCount, cardCount)
	}
	// Disjoint: no card ID appears in both result sets.
	seen := make(map[string]bool, cardCount)
	for _, r := range results {
		for _, c := range r {
			if seen[c.ID] {
				t.Fatalf("card %s was returned by BOTH concurrent calls — the race finding 2 exists to close", c.ID)
			}
			seen[c.ID] = true
			if !want[c.ID] {
				t.Fatalf("unexpected card %s in results", c.ID)
			}
		}
	}
	if len(seen) != cardCount {
		t.Fatalf("union of both result sets = %d cards, want %d (every approved card taken by exactly one caller)", len(seen), cardCount)
	}
	// "One gets all, the other gets none" — the specific disjoint shape
	// the ruling calls out, not just "no overlap in general."
	if !((len(results[0]) == cardCount && len(results[1]) == 0) || (len(results[0]) == 0 && len(results[1]) == cardCount)) {
		t.Fatalf("result sizes = %d and %d, want one call to get all %d and the other 0", len(results[0]), len(results[1]), cardCount)
	}
}
