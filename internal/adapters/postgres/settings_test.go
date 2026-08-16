//go:build integration

package postgres

import (
	"context"
	"strings"
	"testing"

	"github.com/mikeyaustin/jlp/internal/ports/storage"
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

// testSettingsKey returns a key this test (and no one else) owns: a
// "test.settings." prefix that can never collide with a real ai.*
// override key, plus the test's own name, plus suffix for tests that
// need more than one. app_settings is a shared, global table — this
// package's tests run against the SAME dev/CI Postgres an operator (or
// another agent's browser verification) may be actively using via
// /settings, so a test must never assert on the table's TOTAL contents
// or touch a real ai.* key, only on the rows it itself created. See
// find/settingsRows below.
func testSettingsKey(t *testing.T, suffix string) string {
	t.Helper()
	return "test.settings." + strings.ToLower(t.Name()) + "." + suffix
}

// findSetting returns the row for key in rows, if present.
func findSetting(rows []storage.AppSetting, key string) (storage.AppSetting, bool) {
	for _, r := range rows {
		if r.Key == key {
			return r, true
		}
	}
	return storage.AppSetting{}, false
}

// TestSettingsRepositoryListIsEmptyWithNoOverrides pins the "absent
// row means no override" contract at the storage layer: a key this
// test has never Set must not appear in List — config.go's APP_AI_*
// values are never copied into this table (see
// application/settings.Service's own doc comment on why: that would
// silently freeze them). This does NOT assert the whole table is
// empty: app_settings is shared with a running dev/CI stack that may
// already hold real operator overrides, and asserting on the table's
// total contents would make this test fail for anyone who has actually
// used /settings.
func TestSettingsRepositoryListIsEmptyWithNoOverrides(t *testing.T) {
	repo := settingsTestRepo(t)
	key := testSettingsKey(t, "unset")

	rows, err := repo.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, ok := findSetting(rows, key); ok {
		t.Fatalf("List() contains a row for %q, which this test never Set — want it absent until Set is called", key)
	}
}

// TestSettingsRepositorySetGetDeleteRoundTrips exercises the full
// override lifecycle: Set makes a key show up in List, Set again on
// the same key replaces the value (not a duplicate row — the
// migration's ON CONFLICT upsert), and Delete removes it again. Every
// assertion is scoped to THIS test's own key (see testSettingsKey) so
// it neither depends on, nor disturbs, any other row already in the
// shared app_settings table.
func TestSettingsRepositorySetGetDeleteRoundTrips(t *testing.T) {
	repo := settingsTestRepo(t)
	ctx := context.Background()
	key := testSettingsKey(t, "roundtrip")
	// Belt and suspenders: even if an assertion below fails and stops
	// the test partway through its own Set/Delete sequence, this key
	// must never survive to affect a later test run.
	t.Cleanup(func() { _ = repo.Delete(context.Background(), key) })

	if err := repo.Set(ctx, key, "gemma4:latest"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	rows, err := repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if row, ok := findSetting(rows, key); !ok || row.Value != "gemma4:latest" {
		t.Fatalf("List after Set: row for %s = (found=%v, %+v), want gemma4:latest", key, ok, row)
	}

	// Setting the same key again must replace the value, not add a
	// second row — the migration's ON CONFLICT (key) DO UPDATE.
	if err := repo.Set(ctx, key, "gemma4:12b"); err != nil {
		t.Fatalf("Set (replace): %v", err)
	}
	rows, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count := 0
	var got storage.AppSetting
	for _, r := range rows {
		if r.Key == key {
			count++
			got = r
		}
	}
	if count != 1 || got.Value != "gemma4:12b" {
		t.Fatalf("List after replace-Set: %d row(s) for %s (want exactly 1, value gemma4:12b): %+v", count, key, got)
	}

	if err := repo.Delete(ctx, key); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	rows, err = repo.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if _, ok := findSetting(rows, key); ok {
		t.Errorf("List after Delete still contains %s, want absent", key)
	}
}

// TestSettingsRepositoryDeleteAbsentKeyIsNotAnError pins the "reset to
// config" action's contract when there's nothing to reset — a
// no-override row is a perfectly ordinary state (see
// TestSettingsRepositoryListIsEmptyWithNoOverrides), and clicking
// "reset" on a row that's already at config value must not error. Uses
// this test's own reserved key (never a real ai.* one) so it can never
// delete an operator's actual override.
func TestSettingsRepositoryDeleteAbsentKeyIsNotAnError(t *testing.T) {
	repo := settingsTestRepo(t)
	key := testSettingsKey(t, "absent")
	if err := repo.Delete(context.Background(), key); err != nil {
		t.Errorf("Delete(absent key) = %v, want nil", err)
	}
}
