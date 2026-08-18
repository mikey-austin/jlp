package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeVocabRepo is an in-memory storage.VocabularyRepository double for
// the /vocabulary page, /api/v1/vocabulary/events, and /api/v1/words
// (Phase 3 Task 8) tests — the same role fakeLearnerPriorityRepo/
// fakeObservationRepo play for /learner.
type fakeVocabRepo struct {
	items        map[string]*vocabulary.Item
	byID         map[string]*vocabulary.Item
	clientEvents map[string]string
	// deleted mirrors the real table's deleted_at column, keyed by item
	// id: every read below honours it, so a handler test can assert a
	// deleted word really stops reaching the page rather than merely
	// that the delete route returned 303.
	deleted     map[string]time.Time
	nextID      int
	listErr     error
	bulkUpserts [][]storage.WordInput
}

func newFakeVocabRepo() *fakeVocabRepo {
	return &fakeVocabRepo{
		items:        map[string]*vocabulary.Item{},
		byID:         map[string]*vocabulary.Item{},
		clientEvents: map[string]string{},
		deleted:      map[string]time.Time{},
	}
}

func vocabKey(identity learner.IdentityID, expression string) string {
	return string(identity) + "/" + expression
}

func (f *fakeVocabRepo) UpsertOnLookup(_ context.Context, identity learner.IdentityID, expression, reading, meaning, source, _ string, kind vocabulary.Kind, clientEventID string, at time.Time) (vocabulary.Item, bool, error) {
	if clientEventID != "" {
		if itemID, ok := f.clientEvents[string(identity)+"/"+clientEventID]; ok {
			return *f.byID[itemID], true, nil
		}
	}
	k := vocabKey(identity, expression)
	item, ok := f.items[k]
	if !ok {
		f.nextID++
		item = &vocabulary.Item{
			ID:         "vocab-" + string(rune('0'+f.nextID)),
			IdentityID: identity,
			Expression: expression,
			Kind:       kind,
			FirstSeen:  at,
		}
		f.items[k] = item
		f.byID[item.ID] = item
	}
	item.Lookups++
	item.LastEvent = at
	if reading != "" {
		item.Reading = reading
	}
	if meaning != "" {
		item.Meaning = meaning
	}
	if source != "" {
		item.Source = source
	}
	if clientEventID != "" {
		f.clientEvents[string(identity)+"/"+clientEventID] = item.ID
	}
	// Resurrect-on-lookup, mirroring UpsertVocabularyItemOnLookup's
	// "deleted_at = NULL" in db/queries/vocabulary.sql.
	delete(f.deleted, item.ID)
	return *item, false, nil
}

func (f *fakeVocabRepo) RecordProduction(_ context.Context, _ learner.IdentityID, itemID string, successful bool, at time.Time) error {
	item := f.byID[itemID]
	item.Productions++
	if successful {
		item.SuccessfulProductions++
	}
	item.LastEvent = at
	return nil
}

// ListPage pages over List's own result the way the SQL does — sorted by
// (LastEvent DESC, ID DESC), strictly after the cursor, and reporting a
// next cursor only when another row actually follows.
//
// Implemented for real rather than stubbed: a fake that returned nothing
// would make every paging assertion pass against an empty list.
func (f *fakeVocabRepo) ListPage(ctx context.Context, identity learner.IdentityID, filter string, cursor storage.VocabularyCursor, limit int) ([]vocabulary.Item, storage.VocabularyCursor, error) {
	all, err := f.List(ctx, identity, filter)
	if err != nil {
		return nil, storage.VocabularyCursor{}, err
	}
	sort.Slice(all, func(i, j int) bool {
		if !all[i].LastEvent.Equal(all[j].LastEvent) {
			return all[i].LastEvent.After(all[j].LastEvent)
		}
		return all[i].ID > all[j].ID
	})

	var page []vocabulary.Item
	for _, item := range all {
		if !cursor.Zero() {
			after := item.LastEvent.Before(cursor.LastEvent) ||
				(item.LastEvent.Equal(cursor.LastEvent) && item.ID < cursor.ID)
			if !after {
				continue
			}
		}
		page = append(page, item)
		if len(page) > limit {
			break
		}
	}

	var next storage.VocabularyCursor
	if len(page) > limit {
		page = page[:limit]
		last := page[len(page)-1]
		next = storage.VocabularyCursor{LastEvent: last.LastEvent, ID: last.ID}
	}
	return page, next, nil
}

func (f *fakeVocabRepo) List(_ context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []vocabulary.Item
	for _, item := range f.items {
		if item.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
			continue
		}
		if filter == "activate" {
			continue
		}
		if filter == "produced" && item.Productions == 0 {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

func (f *fakeVocabRepo) AllExpressions(_ context.Context, identity learner.IdentityID) (map[string]string, error) {
	out := map[string]string{}
	for _, item := range f.items {
		if item.IdentityID != identity {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
			continue
		}
		out[item.Expression] = item.ID
	}
	return out, nil
}

func (f *fakeVocabRepo) GetByExpressions(_ context.Context, identity learner.IdentityID, expressions []string) ([]vocabulary.Item, error) {
	out := make([]vocabulary.Item, 0, len(expressions))
	for _, expr := range expressions {
		item, ok := f.items[vocabKey(identity, expr)]
		if !ok {
			continue
		}
		if _, gone := f.deleted[item.ID]; gone {
			continue
		}
		out = append(out, *item)
	}
	return out, nil
}

// SoftDelete/Restore mirror the real adapter's contract exactly:
// identity-scoped, idempotent, and ErrNotFound for both "unknown id"
// and "someone else's id".
func (f *fakeVocabRepo) SoftDelete(_ context.Context, identity learner.IdentityID, itemID string, at time.Time) error {
	item, ok := f.byID[itemID]
	if !ok || item.IdentityID != identity {
		return storage.ErrNotFound
	}
	if _, already := f.deleted[itemID]; !already {
		f.deleted[itemID] = at
	}
	return nil
}

func (f *fakeVocabRepo) Restore(_ context.Context, identity learner.IdentityID, itemID string) error {
	item, ok := f.byID[itemID]
	if !ok || item.IdentityID != identity {
		return storage.ErrNotFound
	}
	delete(f.deleted, itemID)
	return nil
}

// isDeleted / exists let a handler test assert on the raw storage state
// rather than on a status code.
func (f *fakeVocabRepo) isDeleted(itemID string) bool {
	_, gone := f.deleted[itemID]
	return gone
}

func (f *fakeVocabRepo) exists(itemID string) bool {
	_, ok := f.byID[itemID]
	return ok
}

// SeedBank mirrors the real adapter's insert-if-absent contract (see
// storage.VocabularyRepository.SeedBank): a no-op per-entry when
// (identity, expression) already has a row. Not exercised by any
// httpx test — implemented for real rather than panicking so this
// fake stays usable if a future test needs it.
func (f *fakeVocabRepo) SeedBank(_ context.Context, identity learner.IdentityID, entries []vocabulary.BankEntry, at time.Time) error {
	for _, e := range entries {
		k := vocabKey(identity, e.Expression)
		if _, ok := f.items[k]; ok {
			continue
		}
		f.nextID++
		item := &vocabulary.Item{
			ID:         "vocab-" + string(rune('0'+f.nextID)),
			IdentityID: identity,
			Expression: e.Expression,
			Reading:    e.Reading,
			Meaning:    e.Meaning,
			Kind:       e.Kind,
			Source:     "expression bank",
			FirstSeen:  at,
			LastEvent:  at,
		}
		f.items[k] = item
		f.byID[item.ID] = item
	}
	return nil
}

// ListActivationCandidates must NOT panic: feedback_test.go's
// feedbackTestServer wires this same fake into a real planner.Planner,
// and RequestFeedback calls its ActivationCandidates (backed by this
// method) on every review round. No httpx test asserts on its
// contents, so it just answers empty — mirroring this fake's
// pre-existing List(filter="activate") behavior, which always
// `continue`d past every item.
func (f *fakeVocabRepo) ListActivationCandidates(context.Context, learner.IdentityID, int) ([]vocabulary.Item, error) {
	return nil, nil
}

// BulkUpsertWords mirrors the real adapter's sparse-merge contract
// (storage.VocabularyRepository.BulkUpsertWords' doc comment) so
// words_test.go can exercise create-then-sparser-update through the
// handler, not just the happy path: empty incoming string fields never
// overwrite existing non-empty values, JLPTLevel 0 never overwrites a
// known level, Tags replaces wholesale only when non-empty, and
// counters are never touched.
func (f *fakeVocabRepo) BulkUpsertWords(_ context.Context, identity learner.IdentityID, words []storage.WordInput, at time.Time) (int, error) {
	f.bulkUpserts = append(f.bulkUpserts, words)
	for _, w := range words {
		k := vocabKey(identity, w.Expression)
		item, ok := f.items[k]
		if !ok {
			f.nextID++
			item = &vocabulary.Item{
				ID:         "vocab-" + string(rune('0'+f.nextID)),
				IdentityID: identity,
				Expression: w.Expression,
				Kind:       vocabulary.KindWord,
				FirstSeen:  at,
			}
			f.items[k] = item
			f.byID[item.ID] = item
		}
		item.LastEvent = at
		if w.Reading != "" {
			item.Reading = w.Reading
		}
		if w.Meaning != "" {
			item.Meaning = w.Meaning
		}
		if w.MeaningEN != "" {
			item.MeaningEN = w.MeaningEN
		}
		if w.JLPTLevel != 0 {
			item.JLPTLevel = w.JLPTLevel
		}
		if w.Source != "" {
			item.Source = w.Source
		}
		if len(w.Tags) > 0 {
			item.Tags = w.Tags
		}
	}
	return len(words), nil
}

func vocabularyTestOptions() (Options, *fakeVocabRepo) {
	opts := testOptions()
	repo := newFakeVocabRepo()
	rec := learning.NewRecorder(newFakeEventRepo(), inprocbus.New())
	opts.Vocabulary = appvocabulary.NewService(repo, rec)
	return opts, repo
}

// TestVocabularyPageRendersItemsWithLookupCountsAndKindLabels covers
// GET /vocabulary: a seeded item renders its expression, reading,
// meaning, lookup count, and a Japanese kind label (not the raw
// "word" enum value).
func TestVocabularyPageRendersItemsWithLookupCountsAndKindLabels(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	repo.items["dev/取り組む"] = &vocabulary.Item{
		ID: "vocab-1", IdentityID: "dev", Expression: "取り組む", Reading: "とりくむ",
		Meaning: "to tackle", Kind: vocabulary.KindWord, Lookups: 3, Productions: 2, SuccessfulProductions: 1,
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vocabulary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vocabulary status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"取り組む", "とりくむ", "to tackle", "単語", "3"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /vocabulary body missing %q: %s", want, body)
		}
	}
}

// TestVocabularyPageFilterTabsRender covers the four filter tabs (すべて
// /調べた/使えた/活性化候補) always rendering, regardless of which filter
// is active.
func TestVocabularyPageFilterTabsRender(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vocabulary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vocabulary status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"すべて", "調べた", "使えた", "活性化候補"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /vocabulary body missing filter tab %q: %s", want, body)
		}
	}
}

// TestVocabularyPageShowsMeaningENAndTagsWhenPresent covers Phase 3
// Task 8: an item imported via POST /api/v1/words (MeaningEN/Tags set)
// shows its English gloss and tags on /vocabulary, while an item that
// only ever came from a vocabulary.lookup event (both zero-value)
// renders those columns empty rather than showing e.g. "<nil>" or "[]".
func TestVocabularyPageShowsMeaningENAndTagsWhenPresent(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	repo.items["dev/勉強"] = &vocabulary.Item{
		ID: "vocab-1", IdentityID: "dev", Expression: "勉強", Reading: "べんきょう",
		Meaning: "学ぶこと", MeaningEN: "studying", Tags: []string{"education", "noun"}, Kind: vocabulary.KindWord,
	}
	repo.items["dev/取り組む"] = &vocabulary.Item{
		ID: "vocab-2", IdentityID: "dev", Expression: "取り組む", Reading: "とりくむ",
		Meaning: "to tackle", Kind: vocabulary.KindWord,
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vocabulary", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /vocabulary status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"studying", "education, noun"} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /vocabulary body missing %q: %s", want, body)
		}
	}
}

// TestVocabularyPageProducedFilterExcludesUnproducedItems covers
// ?filter=produced narrowing the table to items with at least one
// production.
func TestVocabularyPageProducedFilterExcludesUnproducedItems(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	repo.items["dev/取り組む"] = &vocabulary.Item{ID: "vocab-1", IdentityID: "dev", Expression: "取り組む", Productions: 1}
	repo.items["dev/気配"] = &vocabulary.Item{ID: "vocab-2", IdentityID: "dev", Expression: "気配", Productions: 0}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vocabulary?filter=produced", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "取り組む") {
		t.Errorf("body missing produced item 取り組む: %s", body)
	}
	if strings.Contains(body, "気配") {
		t.Errorf("body unexpectedly contains unproduced item 気配: %s", body)
	}
}

func TestVocabularyPageRepositoryErrorReturns500(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	repo.listErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/vocabulary", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestAPIVocabularyIngestCreatesItem covers POST
// /api/v1/vocabulary/events end to end: 201, a snake_case DTO, and the
// item is subsequently visible on the /vocabulary page.
func TestAPIVocabularyIngestCreatesItem(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	body := []byte(`{"type":"vocabulary.lookup","expression":"取り組む","reading":"とりくむ","definition":"to tackle","example":"新しい仕事に取り組む。","source":{"type":"novel","title":"コンビニ人間"},"client_event_id":"evt-1"}`)
	rec := postJSON(t, h, "/api/v1/vocabulary/events", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		ID         string `json:"id"`
		Expression string `json:"expression"`
		Reading    string `json:"reading"`
		Meaning    string `json:"meaning"`
		Kind       string `json:"kind"`
		Source     string `json:"source"`
		Lookups    int    `json:"lookups"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if got.ID == "" {
		t.Fatalf("created item id is empty: %s", rec.Body.String())
	}
	if got.Expression != "取り組む" || got.Reading != "とりくむ" || got.Meaning != "to tackle" {
		t.Fatalf("created item mismatch: %+v", got)
	}
	if got.Source != "novel: コンビニ人間" {
		t.Fatalf("created item Source = %q, want %q", got.Source, "novel: コンビニ人間")
	}
	if got.Lookups != 1 {
		t.Fatalf("created item Lookups = %d, want 1", got.Lookups)
	}

	pageRec := httptest.NewRecorder()
	h.ServeHTTP(pageRec, httptest.NewRequest(http.MethodGet, "/vocabulary", nil))
	if !strings.Contains(pageRec.Body.String(), "取り組む") {
		t.Fatalf("/vocabulary page missing ingested item: %s", pageRec.Body.String())
	}
}

// TestAPIVocabularyIngestMalformedJSONReturnsBadRequest mirrors the
// established "malformed JSON -> 400" API contract.
func TestAPIVocabularyIngestMalformedJSONReturnsBadRequest(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)

	rec := postJSON(t, srv.HandlerForTest(), "/api/v1/vocabulary/events", []byte(`{"type":`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAPIVocabularyIngestUnsupportedTypeReturnsBadRequest: PRD §12
// only supports "vocabulary.lookup" today.
func TestAPIVocabularyIngestUnsupportedTypeReturnsBadRequest(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)

	body := []byte(`{"type":"vocabulary.something-else","expression":"取り組む"}`)
	rec := postJSON(t, srv.HandlerForTest(), "/api/v1/vocabulary/events", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error == "" {
		t.Fatalf("error body missing error field: %s", rec.Body.String())
	}
}
