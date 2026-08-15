//go:build integration

package postgres

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// vocabTestSetup migrates and returns a pool plus a freshly-upserted
// identity — the same "one throwaway identity per test, real uuid
// suffix" pattern priorities_test.go/observations_test.go use to keep
// tests independent of each other and of any dev-seeded data.
func vocabTestSetup(t *testing.T) (*VocabularyRepository, learner.IdentityID) {
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
	identity := learner.Identity{ID: learner.IdentityID("test-vocab-" + uuid.NewString()), DisplayName: "Vocab"}
	if err := identities.Upsert(ctx, identity); err != nil {
		t.Fatal(err)
	}
	return NewVocabularyRepository(pool), identity.ID
}

// TestVocabularyUpsertOnLookupCreatesThenIncrementsCounts pins the
// brief's Step 2 contract: a first lookup creates the item with
// Lookups=1, and a second lookup of the SAME expression (no
// client_event_id) increments Lookups rather than creating a second
// row. It also pins the code-review fix that made IngestEvent's doc
// comment true: the example sentence passed in must actually reach the
// appended vocabulary_events row's payload, not just Reading/Meaning/
// Source (Item itself has no Example column — see
// storage.VocabularyRepository.UpsertOnLookup's doc comment).
func TestVocabularyUpsertOnLookupCreatesThenIncrementsCounts(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	const example = "新しい仕事に取り組む。"

	item, duplicate, err := repo.UpsertOnLookup(ctx, identity, "取り組む", "とりくむ", "to tackle", "novel: コンビニ人間", example, vocabulary.KindWord, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first lookup reported as duplicate")
	}
	if item.Lookups != 1 {
		t.Fatalf("item.Lookups = %d, want 1", item.Lookups)
	}
	if item.Expression != "取り組む" || item.Reading != "とりくむ" || item.Meaning != "to tackle" || item.Source != "novel: コンビニ人間" {
		t.Fatalf("item = %+v, want fields set from the lookup", item)
	}
	if item.FirstSeen.IsZero() || item.LastEvent.IsZero() {
		t.Fatalf("item = %+v, want FirstSeen/LastEvent set", item)
	}

	var rawPayload []byte
	if err := repo.pool.QueryRow(ctx, "SELECT payload FROM vocabulary_events WHERE identity_id = $1 AND item_id = $2", string(identity), item.ID).Scan(&rawPayload); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Reading string `json:"reading"`
		Meaning string `json:"meaning"`
		Source  string `json:"source"`
		Example string `json:"example"`
	}
	if err := json.Unmarshal(rawPayload, &payload); err != nil {
		t.Fatalf("could not decode vocabulary_events.payload %s: %v", rawPayload, err)
	}
	if payload.Example != example {
		t.Fatalf("persisted payload.example = %q, want %q (payload = %s)", payload.Example, example, rawPayload)
	}
	if payload.Reading != "とりくむ" || payload.Meaning != "to tackle" || payload.Source != "novel: コンビニ人間" {
		t.Fatalf("persisted payload = %+v, want reading/meaning/source to also match the lookup", payload)
	}

	second, duplicate, err := repo.UpsertOnLookup(ctx, identity, "取り組む", "", "", "", "", vocabulary.KindWord, "", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("second (distinct) lookup reported as duplicate")
	}
	if second.ID != item.ID {
		t.Fatalf("second.ID = %q, want same item %q (UNIQUE(identity_id, expression))", second.ID, item.ID)
	}
	if second.Lookups != 2 {
		t.Fatalf("second.Lookups = %d, want 2", second.Lookups)
	}
	// Empty reading/meaning/source on the second call must not blank out
	// what the first call set.
	if second.Reading != "とりくむ" || second.Meaning != "to tackle" || second.Source != "novel: コンビニ人間" {
		t.Fatalf("second = %+v, want reading/meaning/source preserved from the first lookup", second)
	}
}

// TestVocabularyUpsertOnLookupClientEventIDIsIdempotent pins the
// partial UNIQUE(identity_id, client_event_id) index: a retried call
// with the SAME client_event_id must return the existing item
// unmodified (duplicate=true, Lookups unchanged) and must not append a
// second vocabulary_events row.
func TestVocabularyUpsertOnLookupClientEventIDIsIdempotent(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	first, duplicate, err := repo.UpsertOnLookup(ctx, identity, "気配", "けはい", "sign, indication", "", "", vocabulary.KindWord, "client-evt-1", now)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first call reported as duplicate")
	}
	if first.Lookups != 1 {
		t.Fatalf("first.Lookups = %d, want 1", first.Lookups)
	}

	second, duplicate, err := repo.UpsertOnLookup(ctx, identity, "気配", "けはい", "sign, indication", "", "", vocabulary.KindWord, "client-evt-1", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate {
		t.Fatal("retried call with the same client_event_id was not reported as a duplicate")
	}
	if second.ID != first.ID || second.Lookups != 1 {
		t.Fatalf("second = %+v, want the same item with Lookups unchanged at 1 (no double-count)", second)
	}

	var eventCount int
	if err := repo.pool.QueryRow(ctx, "SELECT count(*) FROM vocabulary_events WHERE identity_id = $1", string(identity)).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 {
		t.Fatalf("vocabulary_events row count = %d, want 1 (idempotent retry must not append a second row)", eventCount)
	}
}

// TestVocabularyCrossIdentityIsolation: identity B's List/AllExpressions
// must never see identity A's items, even for the identical
// expression string.
func TestVocabularyCrossIdentityIsolation(t *testing.T) {
	repoA, identityA := vocabTestSetup(t)
	identities := NewIdentityRepository(repoA.pool)
	identityB := learner.Identity{ID: learner.IdentityID("test-vocab-b-" + uuid.NewString()), DisplayName: "B"}
	if err := identities.Upsert(context.Background(), identityB); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, _, err := repoA.UpsertOnLookup(ctx, identityA, "気配", "けはい", "sign", "", "", vocabulary.KindWord, "", now); err != nil {
		t.Fatal(err)
	}

	listB, err := repoA.List(ctx, identityB.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 0 {
		t.Fatalf("identity B's List = %+v, want empty (identity A's item must not leak)", listB)
	}
	exprB, err := repoA.AllExpressions(ctx, identityB.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(exprB) != 0 {
		t.Fatalf("identity B's AllExpressions = %+v, want empty", exprB)
	}

	listA, err := repoA.List(ctx, identityA, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 1 || listA[0].Expression != "気配" {
		t.Fatalf("identity A's List = %+v, want exactly the 気配 item", listA)
	}
}

// TestVocabularyRecordProductionIncrementsCounts covers RecordProduction
// updating Productions/SuccessfulProductions independently, and List's
// "produced" filter picking up items with at least one production
// while excluding ones with none.
func TestVocabularyRecordProductionIncrementsCounts(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	produced, _, err := repo.UpsertOnLookup(ctx, identity, "取り組む", "", "", "", "", vocabulary.KindWord, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpsertOnLookup(ctx, identity, "気配", "", "", "", "", vocabulary.KindWord, "", now); err != nil {
		t.Fatal(err) // a second item, never produced — must be excluded by the "produced" filter below
	}

	if err := repo.RecordProduction(ctx, identity, produced.ID, true, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordProduction(ctx, identity, produced.ID, false, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	all, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("List(\"\") = %d items, want 2", len(all))
	}
	var got vocabulary.Item
	for _, item := range all {
		if item.ID == produced.ID {
			got = item
		}
	}
	if got.Productions != 2 || got.SuccessfulProductions != 1 {
		t.Fatalf("produced item = %+v, want Productions=2 SuccessfulProductions=1", got)
	}

	filtered, err := repo.List(ctx, identity, "produced")
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != 1 || filtered[0].ID != produced.ID {
		t.Fatalf("List(\"produced\") = %+v, want exactly the produced item", filtered)
	}

	activate, err := repo.List(ctx, identity, "activate")
	if err != nil {
		t.Fatal(err)
	}
	if len(activate) != 0 {
		t.Fatalf("List(\"activate\") = %+v, want empty (Task 7 wires this filter up)", activate)
	}
}

// TestVocabularyAllExpressionsMapsExpressionToItemID covers the shape
// DetectProduction depends on.
func TestVocabularyAllExpressionsMapsExpressionToItemID(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	item, _, err := repo.UpsertOnLookup(ctx, identity, "取り組む", "", "", "", "", vocabulary.KindWord, "", now)
	if err != nil {
		t.Fatal(err)
	}

	got, err := repo.AllExpressions(ctx, identity)
	if err != nil {
		t.Fatal(err)
	}
	if got["取り組む"] != item.ID {
		t.Fatalf("AllExpressions()[取り組む] = %q, want %q", got["取り組む"], item.ID)
	}
}
