//go:build integration

package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func newTestSession(identity learner.IdentityID, title string) session.Session {
	now := time.Now().UTC().Truncate(time.Microsecond)
	return session.Session{
		ID:         session.ID(uuid.New().String()),
		IdentityID: identity,
		Title:      title,
		Purpose:    "Diary",
		Profile: session.Profile{
			TeacherMode:         "teacher",
			ExplanationLanguage: "both",
			Strictness:          "balanced",
		},
		CreatedAt: now,
		UpdatedAt: now,
	}
}

func TestSessionCreateGetList(t *testing.T) {
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

	// Identities referenced by sessions.identity_id must exist first (FK).
	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-session-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-session-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(ctx, identityA); err != nil {
		t.Fatal(err)
	}
	if err := identities.Upsert(ctx, identityB); err != nil {
		t.Fatal(err)
	}

	repo := NewSessionRepository(pool)

	sA := newTestSession(identityA.ID, "A's session")
	if err := repo.Create(ctx, sA); err != nil {
		t.Fatal(err)
	}
	sB := newTestSession(identityB.ID, "B's session")
	if err := repo.Create(ctx, sB); err != nil {
		t.Fatal(err)
	}

	// Get: owner can fetch their own session, with fields round-tripping.
	got, err := repo.Get(ctx, identityA.ID, sA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Title != sA.Title || got.Purpose != sA.Purpose {
		t.Fatalf("got %+v, want title/purpose to match %+v", got, sA)
	}
	if got.Profile != sA.Profile {
		t.Fatalf("got Profile %+v, want %+v", got.Profile, sA.Profile)
	}

	// Get: cross-identity access returns storage.ErrNotFound.
	_, err = repo.Get(ctx, identityB.ID, sA.ID)
	if !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Get err = %v, want storage.ErrNotFound", err)
	}

	// List: scoped to the caller's identity only.
	listA, err := repo.List(ctx, identityA.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].ID != sA.ID {
		t.Fatalf("List(identityA) = %+v, want only sA", listA)
	}

	listB, err := repo.List(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 1 || listB[0].ID != sB.ID {
		t.Fatalf("List(identityB) = %+v, want only sB", listB)
	}
}
