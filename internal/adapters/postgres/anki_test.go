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

// TestAnkiApprovedForExportThenMarkExported pins the export pipeline's
// full round trip: only "approved" cards come back, MarkExported flips
// exactly those ids to "exported", and calling it again is a safe
// no-op (the "second immediate export is empty" contract
// application/anki.Service.ExportTSV relies on).
func TestAnkiApprovedForExportThenMarkExported(t *testing.T) {
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

	forExport, err := repo.ApprovedForExport(ctx, identity)
	if err != nil {
		t.Fatalf("ApprovedForExport: %v", err)
	}
	if len(forExport) != 1 || forExport[0].ID != approved.ID {
		t.Fatalf("ApprovedForExport = %+v, want exactly [%s] (draft/rejected excluded)", forExport, approved.ID)
	}

	if err := repo.MarkExported(ctx, identity, []string{approved.ID}); err != nil {
		t.Fatalf("MarkExported: %v", err)
	}

	afterMark, err := repo.List(ctx, identity, "exported")
	if err != nil {
		t.Fatal(err)
	}
	if len(afterMark) != 1 || afterMark[0].ID != approved.ID {
		t.Fatalf("List(exported) = %+v, want exactly [%s]", afterMark, approved.ID)
	}

	// Second immediate call: nothing left approved, so ApprovedForExport
	// is empty, and a redundant MarkExported call is a harmless no-op.
	secondRound, err := repo.ApprovedForExport(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(secondRound) != 0 {
		t.Fatalf("ApprovedForExport after marking = %+v, want empty", secondRound)
	}
	if err := repo.MarkExported(ctx, identity, []string{approved.ID}); err != nil {
		t.Fatalf("MarkExported (redundant call): %v", err)
	}
}
