//go:build integration

package postgres

import (
	"context"
	"os"
	"testing"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

func testURL(t *testing.T) string {
	url := os.Getenv("APP_DATABASE_URL")
	if url == "" {
		t.Skip("APP_DATABASE_URL not set")
	}
	return url
}

func TestIdentityUpsertGet(t *testing.T) {
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
	repo := NewIdentityRepository(pool)

	id := learner.Identity{ID: "test-mikey", DisplayName: "美紀", Attributes: map[string]string{"role": "learner"}}
	if err := repo.Upsert(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "美紀" {
		t.Fatalf("got %+v", got)
	}
	if got.Attributes["role"] != "learner" {
		t.Fatalf("got %+v", got)
	}

	// Upsert again with a changed attribute value; Get must reflect the update
	// (pins the fix to the ON CONFLICT clause, which previously dropped attributes).
	id.Attributes = map[string]string{"role": "instructor"}
	if err := repo.Upsert(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err = repo.Get(ctx, id.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Attributes["role"] != "instructor" {
		t.Fatalf("got %+v", got)
	}
}
