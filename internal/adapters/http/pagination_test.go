package httpx

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/vocabulary"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

func TestCursorRoundTrips(t *testing.T) {
	want := storage.VocabularyCursor{
		LastEvent: time.Date(2026, 8, 18, 6, 33, 12, 123456789, time.UTC),
		ID:        "1f0a2b3c-4d5e-6f70-8192-a3b4c5d6e7f8",
	}
	got := decodeCursor(encodeCursor(want))
	if !got.LastEvent.Equal(want.LastEvent) {
		t.Errorf("LastEvent = %v, want %v (nanosecond precision must survive: a bulk import gives many rows timestamps that differ only there)", got.LastEvent, want.LastEvent)
	}
	if got.ID != want.ID {
		t.Errorf("ID = %q, want %q", got.ID, want.ID)
	}
}

// A truncated bookmark or hand-edited URL must show page one, not an
// error page — the learner did not know they were mistreating anything.
func TestGarbageCursorsFallBackToTheFirstPage(t *testing.T) {
	for _, raw := range []string{
		"", "!!!!", "Zm9v", // valid base64, no separator
		encodeCursor(storage.VocabularyCursor{LastEvent: time.Now()}) + "x",
		"bm90LWEtdGltZXwxMjM", // "not-a-time|123"
	} {
		if got := decodeCursor(raw); !got.Zero() {
			t.Errorf("decodeCursor(%q) = %+v, want the zero cursor", raw, got)
		}
	}
}

// The zero cursor must never be encoded into a link: a "next page" URL
// carrying no position is a link back to page one, which reads as a
// stuck button.
func TestNextPageURLIsEmptyOnTheLastPage(t *testing.T) {
	if got := nextPageURL("produced", storage.VocabularyCursor{}); got != "" {
		t.Errorf("nextPageURL with no next cursor = %q, want empty", got)
	}
}

// Paging must not drop the learner back to すべて.
func TestNextPageURLKeepsTheFilter(t *testing.T) {
	next := storage.VocabularyCursor{LastEvent: time.Now(), ID: "abc"}
	got := nextPageURL("activate", next)
	u, err := url.Parse(got)
	if err != nil {
		t.Fatalf("nextPageURL produced an unparseable URL %q: %v", got, err)
	}
	if u.Query().Get("filter") != "activate" {
		t.Errorf("next page URL %q lost the filter", got)
	}
	if u.Query().Get("cursor") == "" {
		t.Errorf("next page URL %q carries no cursor", got)
	}
}

// The end-to-end shape: a full page offers a next link, and following it
// yields DIFFERENT words. The second half is the point — an off-by-one
// in the keyset predicate repeats or skips the boundary row, and only
// comparing the two pages' contents catches it.
func TestVocabularyPagesThroughWithoutRepeatingOrSkipping(t *testing.T) {
	opts, repo := vocabularyTestOptions()
	// More than one page, ALL SHARING A TIMESTAMP: the case that makes
	// the (last_event, id) tiebreaker load-bearing, and exactly what a
	// bulk import produces. A cursor on the timestamp alone would skip
	// every row that shares it.
	same := time.Date(2026, 8, 18, 0, 0, 0, 0, time.UTC)
	for i := range 60 {
		expr := fmt.Sprintf("word-%02d", i)
		repo.items[vocabKey("dev", expr)] = &vocabulary.Item{
			ID:         fmt.Sprintf("vocab-%02d", i),
			IdentityID: "dev",
			Expression: expr,
			Kind:       vocabulary.KindWord,
			Lookups:    1,
			LastEvent:  same,
		}
	}
	h := NewServer(opts).HandlerForTest()

	firstBody, next := fetchVocabPage(t, h, "/vocabulary")
	if next == "" {
		t.Fatal("60 words over a page size of 50 offered no next page")
	}
	secondBody, _ := fetchVocabPage(t, h, next)

	firstWords := wordsOnPage(firstBody)
	secondWords := wordsOnPage(secondBody)
	if len(firstWords) == 0 || len(secondWords) == 0 {
		t.Fatalf("page one had %d words, page two %d — an empty page proves nothing", len(firstWords), len(secondWords))
	}
	for w := range secondWords {
		if firstWords[w] {
			t.Errorf("%q appears on both pages; the cursor is not strictly after the last row", w)
		}
	}
	if total := len(firstWords) + len(secondWords); total != 60 {
		t.Errorf("the two pages hold %d of 60 words; the rest were skipped at the page boundary", total)
	}
}

func fetchVocabPage(t *testing.T, h http.Handler, path string) (body, next string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d", path, rec.Code)
	}
	body = rec.Body.String()
	// Scan EVERY /vocabulary link for the one carrying a cursor. Taking
	// the first match finds a filter tab instead, which is how the first
	// version of this helper concluded there was no next page.
	const marker = `href="/vocabulary?`
	for offset := 0; ; {
		i := strings.Index(body[offset:], marker)
		if i < 0 {
			break
		}
		at := offset + i + len(`href="`)
		offset = at
		j := strings.IndexByte(body[at:], '"')
		if j < 0 {
			break
		}
		if candidate := body[at : at+j]; strings.Contains(candidate, "cursor=") {
			return body, strings.ReplaceAll(candidate, "&amp;", "&")
		}
	}
	return body, ""
}

// wordsOnPage pulls the expression cell out of each row.
func wordsOnPage(body string) map[string]bool {
	out := map[string]bool{}
	// Matches the class, not data-label: naming the cells for the phone
	// card layout changed the attribute order, and this marker silently
	// stopped matching — the test then "passed" its way to an empty page.
	const marker = `class="w-expression" data-label="表現">`
	for offset := 0; ; {
		i := strings.Index(body[offset:], marker)
		if i < 0 {
			return out
		}
		at := offset + i + len(marker)
		offset = at
		end := strings.Index(body[at:], "</td>")
		if end < 0 {
			return out
		}
		out[strings.TrimSpace(body[at:at+end])] = true
	}
}
