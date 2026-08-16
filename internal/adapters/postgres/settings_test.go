//go:build integration

package postgres

import (
	"context"
	"testing"
)

func settingsTestRepo(t *testing.T) *SettingsRepository {
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
	return NewSettingsRepository(pool)
}

// TestSettingsRepositoryListIsEmptyWithNoOverrides pins the "absent
// row means no override" contract at the storage layer: a fresh
// database (no Set calls yet) must report zero rows, not some
// pre-seeded default — config.go's APP_AI_* values are never copied
// into this table (see application/settings.Service's own doc
// comment on why: that would silently freeze them).
func TestSettingsRepositoryListIsEmptyWithNoOverrides(t *testing.T) {
	repo := settingsTestRepo(t)
	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("List() on a fresh database = %v, want empty (no row should exist until Set is called)", rows)
	}
}

// TestSettingsRepositorySetGetDeleteRoundTrips exercises the full
// override lifecycle: Set makes a key show up in List, Set again on
// the same key replaces the value (not a duplicate row — the
// migration's ON CONFLICT upsert), and Delete removes it again.
func TestSettingsRepositorySetGetDeleteRoundTrips(t *testing.T) {
	repo := settingsTestRepo(t)
	ctx := context.Background()

	if err := repo.Set(ctx, "ai.ollama.model", "gemma4:latest"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Key != "ai.ollama.model" || rows[0].Value != "gemma4:latest" {
		t.Fatalf("List after Set = %+v, want exactly one ai.ollama.model=gemma4:latest row", rows)
	}

	// Setting the same key again must replace the value, not add a
	// second row — the migration's ON CONFLICT (key) DO UPDATE.
	if err := repo.Set(ctx, "ai.ollama.model", "gemma4:12b"); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}
	rows, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || rows[0].Value != "gemma4:12b" {
		t.Fatalf("List after replace-Set = %+v, want exactly one ai.ollama.model=gemma4:12b row", rows)
	}

	if err := repo.Delete(ctx, "ai.ollama.model"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rows, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("List after Delete = %v, want empty", rows)
	}
}

// TestSettingsRepositoryDeleteAbsentKeyIsNotAnError pins the "reset to
// config" action's contract when there's nothing to reset — a
// no-override row is a perfectly ordinary state (see
// TestSettingsRepositoryListIsEmptyWithNoOverrides), and clicking
// "reset" on a row that's already at config value must not error.
func TestSettingsRepositoryDeleteAbsentKeyIsNotAnError(t *testing.T) {
	repo := settingsTestRepo(t)
	if err := repo.Delete(context.Background(), "ai.anthropic.model"); err != nil {
		t.Errorf("Delete(absent key) = %v, want nil", err)
	}
}
