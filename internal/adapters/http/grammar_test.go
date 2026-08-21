package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/grammar"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeGrammarPagesRepo is an in-memory storage.GrammarRepository double
// for the /grammar page handlers (list + detail). It's distinct from
// feedback_test.go's fakeGrammarRepo — that one exists only to resolve
// concept tags during feedback requests and panics on every method this
// file needs — so this double is configurable: a test sets stats and/or
// concepts/corrections directly rather than working around fixed
// panicking stubs.
type fakeGrammarPagesRepo struct {
	stats          []storage.ConceptStat
	statsErr       error
	concepts       map[string]grammar.Concept
	corrections    map[string][]storage.CorrectionRecord
	correctionsErr error
}

func newFakeGrammarPagesRepo() *fakeGrammarPagesRepo {
	return &fakeGrammarPagesRepo{
		concepts:    map[string]grammar.Concept{},
		corrections: map[string][]storage.CorrectionRecord{},
	}
}

func (f *fakeGrammarPagesRepo) UpsertConcepts(context.Context, []grammar.Concept) error {
	panic("not used by grammar page tests")
}

func (f *fakeGrammarPagesRepo) ListConcepts(context.Context) ([]grammar.Concept, error) {
	panic("not used by grammar page tests")
}

func (f *fakeGrammarPagesRepo) GetConcept(_ context.Context, slug string) (grammar.Concept, error) {
	c, ok := f.concepts[slug]
	if !ok {
		return grammar.Concept{}, storage.ErrNotFound
	}
	return c, nil
}

func (f *fakeGrammarPagesRepo) ConceptStats(context.Context, learner.IdentityID) ([]storage.ConceptStat, error) {
	if f.statsErr != nil {
		return nil, f.statsErr
	}
	return f.stats, nil
}

func (f *fakeGrammarPagesRepo) CorrectionsForConcept(_ context.Context, _ learner.IdentityID, slug string, limit int) ([]storage.CorrectionRecord, error) {
	if f.correctionsErr != nil {
		return nil, f.correctionsErr
	}
	out := f.corrections[slug]
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func grammarTestOptions() Options {
	opts := testOptions()
	opts.Grammar = newFakeGrammarPagesRepo()
	return opts
}

// TestGrammarListRendersStatRow covers GET /grammar: a seeded
// ConceptStat renders as a row with the concept's name (linked to its
// detail page), JLPT chip, and encounter count.
func TestGrammarListRendersStatRow(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	now := time.Now().UTC()
	repo.stats = []storage.ConceptStat{
		{Slug: "i-adjective-past", Name: "い-adjective past tense (〜かった)", JLPTLevel: 5, Encounters: 3, LastSeen: now},
		{Slug: "particle-wa-topic", Name: "は (topic particle)", JLPTLevel: 5, Encounters: 0},
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /grammar status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"い-adjective past tense (〜かった)",
		`href="/grammar/i-adjective-past"`,
		"N5",
		// The count with its unit: the line answers "have I met this",
		// and a bare 3 in a column read as 3 of something unstated.
		">3回<",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /grammar body missing %q: %s", want, body)
		}
	}
	// A never-encountered concept must not show a formatted zero-time
	// last-seen date.
	if !strings.Contains(body, "未遭遇") {
		t.Errorf("GET /grammar body missing 未遭遇 for a zero-encounter concept: %s", body)
	}
}

// TestGrammarPagesDiscloseTheConversationExclusion pins whole-branch
// review C-1's honesty half. Both grammar surfaces read
// correction_concepts; a conversation turn's corrections are persisted
// as jsonb on conversation_turns and never reach those tables. So a
// concept the tutor corrected out loud renders 「未遭遇 / 遭遇回数 0」on
// the list and an empty 最近の訂正 table on the detail page — a page
// answering "never" to a question whose true answer is "eleven times".
//
// /outcomes already discloses exactly this exclusion
// (outcomes.html.tmpl); these two pages inherited the gap without the
// disclosure. Asserted on the substantive claims, not on a whole
// sentence, so rewording the copy doesn't break the test but deleting
// the disclosure does.
func TestGrammarPagesDiscloseTheConversationExclusion(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	repo.stats = []storage.ConceptStat{
		{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5, Encounters: 0},
	}
	repo.concepts["i-adjective-past"] = grammar.Concept{Slug: "i-adjective-past", Name: "い-adjective past tense", JLPTLevel: 5}
	srv := NewServer(opts)

	for _, path := range []string{"/grammar", "/grammar/i-adjective-past"} {
		rec := httptest.NewRecorder()
		srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200, body=%s", path, rec.Code, rec.Body.String())
		}
		body := rec.Body.String()
		for _, want := range []string{"添削", "会話練習", "音声練習", "含まれません"} {
			if !strings.Contains(body, want) {
				t.Errorf("GET %s never discloses that conversation/speech corrections are excluded (missing %q): %s", path, want, body)
			}
		}
	}
}

func TestGrammarListRepositoryErrorReturns500(t *testing.T) {
	opts := grammarTestOptions()
	opts.Grammar.(*fakeGrammarPagesRepo).statsErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestGrammarDetailRendersExamplesAndCorrection covers GET
// /grammar/{slug}: the concept's description and examples render, plus
// the identity's recent corrections tagged with that concept.
func TestGrammarDetailRendersExamplesAndCorrection(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	repo.concepts["i-adjective-past"] = grammar.Concept{
		Slug:        "i-adjective-past",
		Name:        "い-adjective past tense (〜かった)",
		JLPTLevel:   5,
		Description: "い形容詞の過去形はいをかったに変える。",
		Examples:    []string{"昨日はとても暑かったです。", "テストは難しくなかったです。"},
		Related:     []string{"i-adjective-present"},
	}
	repo.corrections["i-adjective-past"] = []storage.CorrectionRecord{
		{
			ID:            "corr-1",
			Original:      "面白いでした",
			Replacement:   "面白かったです",
			Type:          "conjugation",
			Severity:      "incorrect",
			ExplanationJA: "い形容詞の過去形はかったです。",
			ExplanationEN: "The past tense of an i-adjective is formed with かった.",
			Status:        "presented",
		},
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar/i-adjective-past", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /grammar/i-adjective-past status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"い形容詞の過去形はいをかったに変える。",
		"昨日はとても暑かったです。",
		"テストは難しくなかったです。",
		"面白いでした",
		"面白かったです",
		`href="/grammar/i-adjective-present"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /grammar/i-adjective-past body missing %q: %s", want, body)
		}
	}
}

// TestGrammarDetailUnknownSlugReturnsNotFound: a slug absent from the
// catalog is a 404, not a 500 or an empty-but-200 page.
func TestGrammarDetailUnknownSlugReturnsNotFound(t *testing.T) {
	opts := grammarTestOptions()
	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

func TestGrammarDetailCorrectionsRepositoryErrorReturns500(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	repo.concepts["i-adjective-past"] = grammar.Concept{Slug: "i-adjective-past", Name: "past"}
	repo.correctionsErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar/i-adjective-past", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// The catalog is all 400-odd JLPT concepts. As a four-column table that
// was 400 stacked cards on a phone; the line now answers "which concept,
// how hard, have I met it" and the explanation sits one disclosure deep.
// Every other test on this page passes just as well if the description
// migrates onto the line, so this is the one that holds the shape.
func TestAConceptsExplanationIsBehindTheDisclosureNotOnTheLine(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	repo.stats = []storage.ConceptStat{{
		Slug: "te-form", Name: "て-form", JLPTLevel: 5, Encounters: 3,
		LastSeen:    time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
		Description: "動詞を接続する基本の活用",
		Examples:    []string{"本を読んで、寝ました。", "二つ目の例文。"},
	}}

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar", nil))
	body := rec.Body.String()

	start := strings.Index(body, `class="list__line"`)
	if start < 0 {
		t.Fatalf("no concept rows rendered:\n%s", body)
	}
	end := strings.Index(body[start:], "</summary>")
	if end < 0 {
		t.Fatalf("the row has no summary to close:\n%s", body[start:])
	}
	line := body[start : start+end]
	if !strings.Contains(line, "て-form") {
		t.Fatalf("sliced the wrong element; it does not hold the concept:\n%s", line)
	}

	for _, want := range []string{"て-form", "N5", "3回"} {
		if !strings.Contains(line, want) {
			t.Errorf("the line is missing %q — you cannot find a concept by it:\n%s", want, line)
		}
	}
	for _, hidden := range []string{"動詞を接続する基本の活用", "本を読んで、寝ました。", "2026-08-17"} {
		if strings.Contains(line, hidden) {
			t.Errorf("%q is on the summary line; the list is a wall of prose again:\n%s", hidden, line)
		}
		if !strings.Contains(body, hidden) {
			t.Errorf("%q was dropped from the page entirely, not just off the line", hidden)
		}
	}
	// The row is a taste, not the archive — the second example lives on
	// /grammar/{slug}, or 400 concepts put their whole example list on
	// one page.
	if strings.Contains(body, "二つ目の例文。") {
		t.Error("the row printed every example; only the first belongs on the list page")
	}
}

// A concept never met in 添削 has a zero LastSeen. The detail must say so
// in words rather than formatting 0001-01-01, which is the same trap the
// table had.
func TestAnUnencounteredConceptShowsNoDate(t *testing.T) {
	opts := grammarTestOptions()
	repo := opts.Grammar.(*fakeGrammarPagesRepo)
	repo.stats = []storage.ConceptStat{{Slug: "keigo", Name: "敬語", JLPTLevel: 2, Description: "丁寧な言い方"}}

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/grammar", nil))
	body := rec.Body.String()

	if strings.Contains(body, "0001-01-01") {
		t.Errorf("a never-encountered concept rendered its zero time as a date:\n%s", body)
	}
	if !strings.Contains(body, "未遭遇") {
		t.Errorf("body missing 未遭遇 for a zero-encounter concept:\n%s", body)
	}
}
