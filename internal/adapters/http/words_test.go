package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAPIWordsIngestHappyPathReturns200WithExactContractShape pins the
// external contract byte-for-byte: 200 (NOT 201) with {"imported":N},
// for the OpenAPI example's 3-word batch (one rich, two sparser).
func TestAPIWordsIngestHappyPathReturns200WithExactContractShape(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	body := []byte(`{"words":[
		{"kanji":"勉強","reading":"べんきょう","meaning":"学ぶこと、学習すること","meaning_en":"studying","jlpt_level":3,"tags":["education","noun"],"source":"Anki Deck"},
		{"kanji":"猫","reading":"ねこ","meaning":"cat animal","source":"Anki Deck"},
		{"kanji":"犬","reading":"いぬ","meaning":"dog animal"}
	]}`)
	rec := postJSON(t, h, "/api/v1/words", body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (NOT 201 — matches Nihongo Daily's IngestWordsResponse), body=%s", rec.Code, rec.Body.String())
	}

	var got map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if got["imported"] != 3 {
		t.Fatalf("imported = %d, want 3 (body=%s)", got["imported"], rec.Body.String())
	}
	// Exact key: the wire contract is "imported", not e.g. "count".
	if !strings.Contains(rec.Body.String(), `"imported"`) {
		t.Fatalf("body missing exact key \"imported\": %s", rec.Body.String())
	}
}

// TestAPIWordsIngestSparseResyncPreservesRicherData exercises the
// handler end-to-end against the sparse-merge fakeVocabRepo: a second,
// sparser POST for an already-imported word still returns 200
// {"imported":1} and does not erase the richer fields from the first
// import.
func TestAPIWordsIngestSparseResyncPreservesRicherData(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	first := []byte(`{"words":[{"kanji":"勉強","reading":"べんきょう","meaning":"学ぶこと","meaning_en":"studying","jlpt_level":3,"tags":["education"],"source":"Anki Deck"}]}`)
	rec := postJSON(t, h, "/api/v1/words", first)
	if rec.Code != http.StatusOK {
		t.Fatalf("first POST status = %d, body=%s", rec.Code, rec.Body.String())
	}

	sparse := []byte(`{"words":[{"kanji":"勉強","reading":"べんきょう","meaning":"学ぶこと"}]}`)
	rec = postJSON(t, h, "/api/v1/words", sparse)
	if rec.Code != http.StatusOK {
		t.Fatalf("second POST status = %d, body=%s", rec.Code, rec.Body.String())
	}
	var got map[string]int
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response %s: %v", rec.Body.String(), err)
	}
	if got["imported"] != 1 {
		t.Fatalf("imported = %d, want 1", got["imported"])
	}

	item := repo.items[vocabKey("dev", "勉強")]
	if item == nil {
		t.Fatal("勉強 not found after import")
	}
	if item.MeaningEN != "studying" {
		t.Fatalf("MeaningEN = %q, want \"studying\" preserved from the first, richer import", item.MeaningEN)
	}
	if item.JLPTLevel != 3 {
		t.Fatalf("JLPTLevel = %d, want 3 preserved", item.JLPTLevel)
	}
	if len(item.Tags) != 1 || item.Tags[0] != "education" {
		t.Fatalf("Tags = %#v, want [education] preserved", item.Tags)
	}
	if item.Lookups != 0 {
		t.Fatalf("Lookups = %d, want 0 — a sync is not a lookup event", item.Lookups)
	}
}

// TestAPIWordsIngestMalformedJSONReturns400 covers a body that isn't
// valid JSON at all.
func TestAPIWordsIngestMalformedJSONReturns400(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	rec := postJSON(t, h, "/api/v1/words", []byte(`{not json`))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("body missing {\"error\":...}: %s", rec.Body.String())
	}
}

// TestAPIWordsIngestValidationErrorReturns400WithMessage covers a
// missing required field (kanji here): the service's *WordValidationError
// message (naming the index) must surface verbatim as the 400 body's
// "error" field.
func TestAPIWordsIngestValidationErrorReturns400WithMessage(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	body := []byte(`{"words":[{"kanji":"","reading":"ねこ","meaning":"cat"}]}`)
	rec := postJSON(t, h, "/api/v1/words", body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if !strings.Contains(got.Error, "words[0]") {
		t.Fatalf("error = %q, want it to name index 0", got.Error)
	}
}

// TestAPIWordsIngestOversizedBatchReturns400 covers the brief's
// >1000-words case: the exact wire message is pinned by the brief, not
// just "some 400".
func TestAPIWordsIngestOversizedBatchReturns400(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	var sb strings.Builder
	sb.WriteString(`{"words":[`)
	for i := 0; i < 1001; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `{"kanji":"w%d","reading":"r","meaning":"m"}`, i)
	}
	sb.WriteString(`]}`)

	rec := postJSON(t, h, "/api/v1/words", []byte(sb.String()))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body=%s", rec.Code, rec.Body.String())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error != "too many words in one request (max 1000)" {
		t.Fatalf("error = %q, want the brief's exact wire message", got.Error)
	}
}

// TestAPIWordsIngestOversizedBodyReturns400 covers the 4 MiB body cap
// (larger than the API's default 1 MiB) with the brief's exact
// "request body too large" message — distinct from the generic
// "malformed request body" other decode failures get.
func TestAPIWordsIngestOversizedBodyReturns400(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	srv := NewServer(opts)
	h := srv.HandlerForTest()

	oversized := []byte(`{"words":[{"kanji":"` + strings.Repeat("a", maxWordsRequestBodyBytes+1) + `","reading":"r","meaning":"m"}]}`)
	rec := postJSON(t, h, "/api/v1/words", oversized)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body len=%d", rec.Code, rec.Body.Len())
	}
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode error body %s: %v", rec.Body.String(), err)
	}
	if got.Error != "request body too large" {
		t.Fatalf("error = %q, want %q", got.Error, "request body too large")
	}
}

// TestAPIWordsIngestUnauthenticatedReturnsJSON401 covers the existing
// RequireIdentity middleware coverage — no special-casing was added
// for this route, so it must reject the same way every other
// /api/v1 route does.
func TestAPIWordsIngestUnauthenticatedReturnsJSON401(t *testing.T) {
	opts, _ := vocabularyTestOptions()
	opts.Auth = fakeAuth{err: errors.New("no session cookie")}
	srv := NewServer(opts)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/words", strings.NewReader(`{"words":[]}`))
	req.Header.Set("Content-Type", "application/json")
	srv.HandlerForTest().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401, body=%s", rec.Code, rec.Body.String())
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}
}
