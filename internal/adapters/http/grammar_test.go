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
		">3<",
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
