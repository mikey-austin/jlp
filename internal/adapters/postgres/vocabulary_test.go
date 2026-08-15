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

	// Neither seeded item qualifies for "activate" (Task 7, PRD
	// §55/§17.5): 取り組む is kind=word with Lookups=1 (< 3) and
	// Productions=2 (!= 0); 気配 is kind=word with Productions=0 but
	// Lookups=1 (< 3) — see TestVocabularyActivateFilterMatchesEitherClause
	// below for the filter actually matching something.
	activate, err := repo.List(ctx, identity, "activate")
	if err != nil {
		t.Fatal(err)
	}
	if len(activate) != 0 {
		t.Fatalf("List(\"activate\") = %+v, want empty (neither seeded item matches the filter)", activate)
	}
}

// TestVocabularyActivateFilterMatchesEitherClause pins Task 7's exact
// activation-candidate condition (PRD §55/§17.5): (Lookups >= 3 AND
// Productions = 0) OR (Kind IN (expression, pattern) AND Productions =
// 0) — five items, one per boundary, prove both clauses independently
// and that Productions != 0 excludes an item from EITHER clause.
func TestVocabularyActivateFilterMatchesEitherClause(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	// A: kind=word, Lookups=3, Productions=0 -> matches clause 1.
	if _, _, err := repo.UpsertOnLookup(ctx, identity, "見送る", "", "", "", "", vocabulary.KindWord, "", now); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ { // two more lookups -> Lookups=3
		if _, _, err := repo.UpsertOnLookup(ctx, identity, "見送る", "", "", "", "", vocabulary.KindWord, "", now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	// B: kind=word, Lookups=2, Productions=0 -> matches neither clause
	// (below the Lookups>=3 floor, and not a bank kind).
	if _, _, err := repo.UpsertOnLookup(ctx, identity, "様子", "", "", "", "", vocabulary.KindWord, "", now); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.UpsertOnLookup(ctx, identity, "様子", "", "", "", "", vocabulary.KindWord, "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// C: a freshly-seeded bank item, kind=expression, Lookups=0,
	// Productions=0 -> matches clause 2 from the moment it's seeded,
	// with no lookups of its own at all.
	if err := repo.SeedBank(ctx, identity, []vocabulary.BankEntry{{Expression: "それはそれとして", Meaning: "that aside", Kind: vocabulary.KindExpression}}, now); err != nil {
		t.Fatal(err)
	}

	// D: kind=pattern, Lookups=3, but Productions=1 -> excluded from
	// BOTH clauses despite satisfying the Lookups>=3 floor, because
	// Productions != 0.
	itemD, _, err := repo.UpsertOnLookup(ctx, identity, "〜に越したことはない", "", "", "", "", vocabulary.KindPattern, "", now)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, _, err := repo.UpsertOnLookup(ctx, identity, "〜に越したことはない", "", "", "", "", vocabulary.KindPattern, "", now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.RecordProduction(ctx, identity, itemD.ID, true, now.Add(5*time.Minute)); err != nil {
		t.Fatal(err)
	}

	activate, err := repo.List(ctx, identity, "activate")
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[string]bool, len(activate))
	for _, item := range activate {
		got[item.Expression] = true
	}
	if len(activate) != 2 || !got["見送る"] || !got["それはそれとして"] {
		t.Fatalf("List(\"activate\") = %+v, want exactly [見送る, それはそれとして]", activate)
	}
}

// TestVocabularyListActivationCandidatesOrdersAndLimitsInSQL pins the
// code-review fix that moved ORDER BY lookups DESC / LIMIT out of
// application/planner.Planner.ActivationCandidates (a Go-side sort over
// List's output) and into this dedicated query — the same convention
// PriorityRepository.Top already uses. Three bank items with distinct
// Lookups counts, requested with limit=2, must come back highest-first
// and capped.
func TestVocabularyListActivationCandidatesOrdersAndLimitsInSQL(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	entries := []vocabulary.BankEntry{
		{Expression: "それはそれとして", Meaning: "m1", Kind: vocabulary.KindExpression},
		{Expression: "〜に越したことはない", Meaning: "m2", Kind: vocabulary.KindPattern},
		{Expression: "気が置けない", Meaning: "m3", Kind: vocabulary.KindExpression},
	}
	if err := repo.SeedBank(ctx, identity, entries, now); err != nil {
		t.Fatal(err)
	}
	// Bump lookups for two of the three (bank items start at 0) via real
	// lookups, giving them distinct, orderable counts; the third stays
	// at 0 and should be excluded once limit=2 caps the result.
	for i := 0; i < 5; i++ {
		if _, _, err := repo.UpsertOnLookup(ctx, identity, "それはそれとして", "", "", "", "", vocabulary.KindExpression, "", now.Add(time.Duration(i+1)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, _, err := repo.UpsertOnLookup(ctx, identity, "〜に越したことはない", "", "", "", "", vocabulary.KindPattern, "", now.Add(time.Duration(i+10)*time.Minute)); err != nil {
			t.Fatal(err)
		}
	}

	got, err := repo.ListActivationCandidates(ctx, identity, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("ListActivationCandidates(limit=2) = %+v, want exactly 2 items", got)
	}
	if got[0].Expression != "それはそれとして" || got[0].Lookups != 5 {
		t.Fatalf("got[0] = %+v, want それはそれとして with Lookups=5 (highest)", got[0])
	}
	if got[1].Expression != "〜に越したことはない" || got[1].Lookups != 2 {
		t.Fatalf("got[1] = %+v, want 〜に越したことはない with Lookups=2", got[1])
	}

	all, err := repo.ListActivationCandidates(ctx, identity, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ListActivationCandidates(limit=0) = %+v, want all 3 items (unlimited)", all)
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

// TestVocabularySeedBankInsertsOnceAndNeverResetsCounts pins the
// brief's seed-idempotency requirement (Task 7, PRD §55/§17.5): a first
// SeedBank call creates a zero-count baseline row; once the learner has
// actually looked the expression up for real (Lookups bumped by
// UpsertOnLookup), a SECOND SeedBank call for the same expression —
// exactly what a `jlp seed` rerun does — must be a silent no-op: no
// duplicate row, and critically, Lookups must NOT be reset back to 0.
// This is what distinguishes SeedBank from UpsertOnLookup, whose own ON
// CONFLICT DO UPDATE would have clobbered it.
func TestVocabularySeedBankInsertsOnceAndNeverResetsCounts(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	entry := []vocabulary.BankEntry{{Expression: "それはそれとして", Meaning: "that aside; setting that aside", Kind: vocabulary.KindExpression}}

	if err := repo.SeedBank(ctx, identity, entry, now); err != nil {
		t.Fatal(err)
	}
	first, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("List after first seed = %+v, want exactly 1 item", first)
	}
	if first[0].Lookups != 0 || first[0].Productions != 0 {
		t.Fatalf("seeded item = %+v, want Lookups=0 Productions=0 (bank baseline)", first[0])
	}
	if first[0].Kind != vocabulary.KindExpression {
		t.Fatalf("seeded item Kind = %q, want %q", first[0].Kind, vocabulary.KindExpression)
	}

	// Simulate real learner usage: a genuine lookup bumps Lookups to 1.
	if _, _, err := repo.UpsertOnLookup(ctx, identity, "それはそれとして", "", "", "", "", vocabulary.KindExpression, "", now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	// Re-seed (simulating a second `jlp seed` run) — must be a no-op.
	if err := repo.SeedBank(ctx, identity, entry, now.Add(2*time.Minute)); err != nil {
		t.Fatal(err)
	}

	second, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 {
		t.Fatalf("List after re-seed = %+v, want still exactly 1 item (no duplicate row)", second)
	}
	if second[0].Lookups != 1 {
		t.Fatalf("re-seed reset Lookups to %d, want 1 preserved from the real lookup in between", second[0].Lookups)
	}
	if second[0].ID != first[0].ID {
		t.Fatalf("re-seed produced a different item ID: %q vs original %q", second[0].ID, first[0].ID)
	}
}

// TestVocabularySeedBankDoesNotOverwriteAnExistingLookedUpItem covers
// the other order: an expression the learner already looked up BEFORE
// it was ever in the bank (e.g. they encountered it naturally, then a
// later `data/expressions/core.yaml` edit adds it) must keep its real
// Reading/Meaning/Source from that lookup — SeedBank must not silently
// overwrite them with the bank's own values.
func TestVocabularySeedBankDoesNotOverwriteAnExistingLookedUpItem(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	item, _, err := repo.UpsertOnLookup(ctx, identity, "気が置けない", "きがおけない", "learner's own note", "novel: 何か", "", vocabulary.KindExpression, "", now)
	if err != nil {
		t.Fatal(err)
	}

	entry := []vocabulary.BankEntry{{Expression: "気が置けない", Reading: "きがおけない", Meaning: "bank meaning", Kind: vocabulary.KindExpression}}
	if err := repo.SeedBank(ctx, identity, entry, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	all, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("List = %+v, want exactly 1 item (no duplicate)", all)
	}
	if all[0].ID != item.ID || all[0].Lookups != 1 || all[0].Meaning != "learner's own note" {
		t.Fatalf("item = %+v, want the learner's original lookup untouched by the later seed", all[0])
	}
}

// TestVocabularySeedBankInsertsMultipleEntriesInOneTransaction pins the
// code-review fix that made SeedBank bulk (mirroring
// GrammarRepository.UpsertConcepts' shape) instead of one transaction
// per entry: a single call with several entries — one brand new, one
// already present from a prior real lookup — commits every new entry
// and leaves the pre-existing one untouched, all in the SAME call.
func TestVocabularySeedBankInsertsMultipleEntriesInOneTransaction(t *testing.T) {
	repo, identity := vocabTestSetup(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	if _, _, err := repo.UpsertOnLookup(ctx, identity, "腹が立つ", "はらがたつ", "learner's own note", "", "", vocabulary.KindExpression, "", now); err != nil {
		t.Fatal(err)
	}

	entries := []vocabulary.BankEntry{
		{Expression: "それはそれとして", Meaning: "that aside", Kind: vocabulary.KindExpression},
		{Expression: "腹が立つ", Reading: "はらがたつ", Meaning: "bank meaning (should not apply)", Kind: vocabulary.KindExpression},
		{Expression: "〜に越したことはない", Reading: "にこしたことはない", Meaning: "there's nothing better than", Kind: vocabulary.KindPattern},
	}
	if err := repo.SeedBank(ctx, identity, entries, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}

	all, err := repo.List(ctx, identity, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("List = %+v, want exactly 3 items (1 pre-existing + 2 newly seeded)", all)
	}
	byExpr := make(map[string]vocabulary.Item, len(all))
	for _, item := range all {
		byExpr[item.Expression] = item
	}
	if got := byExpr["腹が立つ"]; got.Lookups != 1 || got.Meaning != "learner's own note" {
		t.Fatalf("腹が立つ = %+v, want the pre-existing lookup untouched by the batch seed", got)
	}
	if got := byExpr["それはそれとして"]; got.Lookups != 0 || got.Meaning != "that aside" {
		t.Fatalf("それはそれとして = %+v, want a fresh bank baseline", got)
	}
	if got := byExpr["〜に越したことはない"]; got.Kind != vocabulary.KindPattern {
		t.Fatalf("〜に越したことはない = %+v, want Kind=pattern", got)
	}
}
