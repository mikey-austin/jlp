package httpx

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/inprocbus"
	"github.com/mikeyaustin/jlp/internal/application/learning"
	appvocabulary "github.com/mikeyaustin/jlp/internal/application/vocabulary"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
)

// fakeVocabRepo is an in-memory storage.VocabularyRepository double for
// the /vocabulary page and /api/v1/vocabulary/events tests — the same
// role fakeLearnerPriorityRepo/fakeObservationRepo play for /learner.
type fakeVocabRepo struct {
	items        map[string]*vocabulary.Item
	byID         map[string]*vocabulary.Item
	clientEvents map[string]string
	nextID       int
	listErr      error
}

func newFakeVocabRepo() *fakeVocabRepo {
	return &fakeVocabRepo{
		items:        map[string]*vocabulary.Item{},
		byID:         map[string]*vocabulary.Item{},
		clientEvents: map[string]string{},
	}
}

func vocabKey(identity learner.IdentityID, expression string) string {
	return string(identity) + "/" + expression
}

func (f *fakeVocabRepo) UpsertOnLookup(_ context.Context, identity learner.IdentityID, expression, reading, meaning, source string, kind vocabulary.Kind, clientEventID string, at time.Time) (vocabulary.Item, bool, error) {
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

func (f *fakeVocabRepo) List(_ context.Context, identity learner.IdentityID, filter string) ([]vocabulary.Item, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	var out []vocabulary.Item
	for _, item := range f.items {
		if item.IdentityID != identity {
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
		if item.IdentityID == identity {
			out[item.Expression] = item.ID
		}
	}
	return out, nil
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
