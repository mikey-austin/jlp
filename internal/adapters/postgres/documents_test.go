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

func TestDocumentGetOrCreateSaveAndVersions(t *testing.T) {
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

	// FK chain: identities -> sessions -> documents.
	identities := NewIdentityRepository(pool)
	identityA := learner.Identity{ID: learner.IdentityID("test-doc-a-" + uuid.NewString()), DisplayName: "A"}
	identityB := learner.Identity{ID: learner.IdentityID("test-doc-b-" + uuid.NewString()), DisplayName: "B"}
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

	docs := NewDocumentRepository(pool)

	// GetOrCreateForSession: first call creates an empty document at version 1.
	doc, err := docs.GetOrCreateForSession(ctx, identityA.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if doc.Version != 1 {
		t.Fatalf("fresh document Version = %d, want 1", doc.Version)
	}
	if doc.Content != "" {
		t.Fatalf("fresh document Content = %q, want empty", doc.Content)
	}

	// GetOrCreateForSession: second call returns the same document, not a
	// new one (the unique index on session_id backs this, too).
	again, err := docs.GetOrCreateForSession(ctx, identityA.ID, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != doc.ID {
		t.Fatalf("GetOrCreateForSession returned a different document on second call: %q != %q", again.ID, doc.ID)
	}

	// Save: increments Version and appends a document_versions row, atomically.
	saved1, err := docs.Save(ctx, identityA.ID, doc.ID, "昨日、映画を見た。")
	if err != nil {
		t.Fatal(err)
	}
	if saved1.Version != 2 {
		t.Fatalf("after first Save, Version = %d, want 2", saved1.Version)
	}
	if saved1.Content != "昨日、映画を見た。" {
		t.Fatalf("after first Save, Content = %q, want the saved text", saved1.Content)
	}

	saved2, err := docs.Save(ctx, identityA.ID, doc.ID, "昨日、映画を見た。とても面白かった。")
	if err != nil {
		t.Fatal(err)
	}
	if saved2.Version != 3 {
		t.Fatalf("after second Save, Version = %d, want 3", saved2.Version)
	}

	versions, err := docs.ListVersions(ctx, identityA.ID, doc.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("ListVersions returned %d rows, want 2 (one per Save)", len(versions))
	}
	// Newest first.
	if versions[0].Version != 3 || versions[1].Version != 2 {
		t.Fatalf("ListVersions order = [%d,%d], want [3,2]", versions[0].Version, versions[1].Version)
	}

	// Get: owner can fetch the document directly by ID.
	got, err := docs.Get(ctx, identityA.ID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != 3 || got.Content != saved2.Content {
		t.Fatalf("Get = %+v, want Version=3 Content=%q", got, saved2.Content)
	}

	// Cross-identity access must miss, not leak another learner's document.
	if _, err := docs.Get(ctx, identityB.ID, doc.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Get err = %v, want storage.ErrNotFound", err)
	}
	if _, err := docs.Save(ctx, identityB.ID, doc.ID, "mallory's text"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("cross-identity Save err = %v, want storage.ErrNotFound", err)
	}

	// A cross-identity Save must not have mutated the document or appended
	// a version row.
	unchanged, err := docs.Get(ctx, identityA.ID, doc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Version != 3 {
		t.Fatalf("after cross-identity Save attempt, Version = %d, want unchanged 3", unchanged.Version)
	}
	versionsAfter, err := docs.ListVersions(ctx, identityA.ID, doc.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(versionsAfter) != 2 {
		t.Fatalf("after cross-identity Save attempt, ListVersions returned %d rows, want unchanged 2", len(versionsAfter))
	}
}
