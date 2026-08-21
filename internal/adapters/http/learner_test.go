package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/analytics"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/learnermodel"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeLearnerPriorityRepo is an in-memory storage.PriorityRepository
// double for the /learner page and /api/v1/learner/priorities tests.
// It's distinct from feedback_test.go's fakePriorityRepo — that one
// exists only to satisfy feedback.Service's construction (always
// answers Top empty, panics on ReplaceAll) — because these tests need
// to seed and inspect real rows, the same "page tests get a
// configurable double, pipeline tests get a fixed stub" split
// server_test.go's fakeGrammarPagesRepo/feedback_test.go's
// fakeGrammarRepo already establishes.
type fakeLearnerPriorityRepo struct {
	top    []storage.Priority
	topErr error
}

func (f *fakeLearnerPriorityRepo) ReplaceAll(context.Context, learner.IdentityID, []storage.Priority) error {
	panic("not used by learner page/api tests")
}

func (f *fakeLearnerPriorityRepo) Top(_ context.Context, _ learner.IdentityID, limit int) ([]storage.Priority, error) {
	if f.topErr != nil {
		return nil, f.topErr
	}
	out := f.top
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// fakeObservationRepo is an in-memory storage.ObservationRepository
// double for the /learner page tests.
type fakeObservationRepo struct {
	list    []learnermodel.Observation
	listErr error
}

func (f *fakeObservationRepo) Upsert(context.Context, learnermodel.Observation) error {
	panic("not used by learner page tests")
}

func (f *fakeObservationRepo) List(context.Context, learner.IdentityID) ([]learnermodel.Observation, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.list, nil
}

func (f *fakeObservationRepo) DeleteAll(context.Context, learner.IdentityID) error {
	panic("not used by learner page tests")
}

// fakeLearnerRetrievalRepo is an in-memory storage.RetrievalRepository
// double for the /learner page's 復習キュー table tests — only List is
// exercised (learnerPage's own only call), every other method panics.
type fakeLearnerRetrievalRepo struct {
	list    []storage.RetrievalItem
	listErr error
}

func (f *fakeLearnerRetrievalRepo) Upsert(context.Context, storage.RetrievalItem) error {
	panic("not used by learner page tests")
}

func (f *fakeLearnerRetrievalRepo) Get(context.Context, learner.IdentityID, string, string) (storage.RetrievalItem, error) {
	panic("not used by learner page tests")
}

func (f *fakeLearnerRetrievalRepo) Due(context.Context, learner.IdentityID, time.Time, int) ([]storage.RetrievalItem, error) {
	panic("not used by learner page tests")
}

func (f *fakeLearnerRetrievalRepo) List(_ context.Context, _ learner.IdentityID, limit int) ([]storage.RetrievalItem, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := f.list
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func learnerTestOptions() Options {
	opts := testOptions()
	opts.Priorities = &fakeLearnerPriorityRepo{}
	opts.Observations = &fakeObservationRepo{}
	opts.Retrieval = &fakeLearnerRetrievalRepo{}
	// Zero-value AgentUsage/SystemStats by default; tests exercising
	// those sections override this with their own fakeAnalyticsRepo (see
	// TestLearnerPageRendersAgentUsageAndSystemSections).
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{})
	return opts
}

// TestLearnerPageRendersPriorityAndObservationRows covers GET
// /learner: a seeded priority renders in the priority table (subject,
// type, score, reason) and a seeded observation renders in the
// observations list.
func TestLearnerPageRendersPriorityAndObservationRows(t *testing.T) {
	opts := learnerTestOptions()
	opts.Priorities.(*fakeLearnerPriorityRepo).top = []storage.Priority{
		{SubjectType: "concept", Subject: "i-adjective-past", Score: 7.5, Reason: "recurring weakness: 5 occurrences in 30d"},
	}
	now := time.Now().UTC()
	opts.Observations.(*fakeObservationRepo).list = []learnermodel.Observation{
		{ID: "obs-1", SubjectType: learnermodel.SubjectConcept, Subject: "i-adjective-past", Kind: learnermodel.KindWeakness, Confidence: 1.0, UpdatedAt: now},
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /learner status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"i-adjective-past",
		"7.5",
		"recurring weakness: 5 occurrences in 30d",
		"weakness",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /learner body missing %q: %s", want, body)
		}
	}
}

// TestLearnerReviewQueueDisclosesTheSpokenGrammarGap pins whole-branch
// review C-1's functional half. Task 7's scheduler subscribes to
// quiz.answered / correction.retried / vocabulary.produced-correctly; a
// conversation correction has no retry route at all, so
// correction.retried can never fire for one and a grammar concept the
// learner got wrong IN SPEECH is never scheduled. Vocabulary IS covered
// through the same conversation path, which is precisely what makes the
// grammar hole look like a working feature with nothing due.
func TestLearnerReviewQueueDisclosesTheSpokenGrammarGap(t *testing.T) {
	srv := NewServer(learnerTestOptions())
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /learner status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"復習キュー", "会話練習", "音声練習", "文法"} {
		if !strings.Contains(body, want) {
			t.Errorf("/learner's review queue never says spoken grammar isn't scheduled (missing %q): %s", want, body)
		}
	}
}

func TestLearnerPagePrioritiesRepositoryErrorReturns500(t *testing.T) {
	opts := learnerTestOptions()
	opts.Priorities.(*fakeLearnerPriorityRepo).topErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestLearnerPageObservationsRepositoryErrorReturns500(t *testing.T) {
	opts := learnerTestOptions()
	opts.Observations.(*fakeObservationRepo).listErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestLearnerPageRendersRetrievalQueueRows covers Phase 4 Task 7's
// 復習キュー table: a seeded retrieval item renders its subject, subject
// type, due date, and interval (in whole days).
func TestLearnerPageRendersRetrievalQueueRows(t *testing.T) {
	opts := learnerTestOptions()
	dueAt := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	opts.Retrieval.(*fakeLearnerRetrievalRepo).list = []storage.RetrievalItem{
		{SubjectType: "expression", Subject: "積もる", DueAt: dueAt, Interval: 3 * 24 * time.Hour, Successes: 2, Failures: 0},
	}

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /learner status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"復習キュー",
		"積もる",
		"expression",
		"2026-08-20",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /learner body missing %q: %s", want, body)
		}
	}
}

func TestLearnerPageRetrievalRepositoryErrorReturns500(t *testing.T) {
	opts := learnerTestOptions()
	opts.Retrieval.(*fakeLearnerRetrievalRepo).listErr = context.DeadlineExceeded

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// TestLearnerPageRendersAgentUsageAndSystemSections covers the Task 7
// additions: エージェント利用 rows (including the blank-agent 未分類
// fallback for pre-migration ai_requests rows) and システム totals.
func TestLearnerPageRendersAgentUsageAndSystemSections(t *testing.T) {
	opts := learnerTestOptions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{
		agentUsage: []storage.AgentUsage{
			{Agent: "teacher", Requests: 5, Successes: 4, AvgLatencyMS: 420},
			{Agent: "", Requests: 1, Successes: 1, AvgLatencyMS: 100},
		},
		system: storage.SystemStats{
			LearningEvents: 12,
			EventsByType:   map[string]int{"correction.presented": 9, "grammar.concept.encountered": 3},
			AIRequests:     6,
		},
	})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /learner status = %d, want 200, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"エージェント利用",
		"teacher",
		"420",
		"未分類", // the blank-agent row's display label.
		"システム",
		"12",
		"correction.presented",
		"6",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("GET /learner body missing %q: %s", want, body)
		}
	}
}

func TestLearnerPageAgentUsageRepositoryErrorReturns500(t *testing.T) {
	opts := learnerTestOptions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{agentUsageErr: context.DeadlineExceeded})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestLearnerPageSystemStatsRepositoryErrorReturns500(t *testing.T) {
	opts := learnerTestOptions()
	opts.Analytics = analytics.NewService(fakeAnalyticsRepo{systemErr: context.DeadlineExceeded})

	srv := NewServer(opts)
	rec := httptest.NewRecorder()
	srv.HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

// A due word's subject is its vocabulary UUID. Showing that raw tells
// the learner nothing about what to revise, and 復習キュー only started
// carrying words when 練習 began scheduling them.
func TestLearnerPageShowsTheWordBehindADueExpression(t *testing.T) {
	opts := learnerTestOptions()
	opts.Retrieval.(*fakeLearnerRetrievalRepo).list = []storage.RetrievalItem{
		{SubjectType: "expression", Subject: "11111111-1111-1111-1111-111111111111",
			DueAt: time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC), Interval: 24 * time.Hour},
	}
	opts.Vocabulary = nil // the label path must survive an unwired service

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — a nil vocabulary service must not 500 the page that shows everything else", rec.Code)
	}
	// With nothing to resolve it, the raw subject is the honest fallback:
	// a blank cell would be worse than an unfriendly one.
	if !strings.Contains(rec.Body.String(), "11111111-1111-1111-1111-111111111111") {
		t.Error("an unresolvable subject rendered as neither its label nor its raw value")
	}
}

// The page used to open every table at once, so a phone reader scrolled
// past four screens of scoring diagnostics to reach the numbers the page
// is opened for. Sections are disclosures now, and the two a learner
// acts on are the ones that start open.
func TestLearnerPageOpensOnlyTheActionableSections(t *testing.T) {
	opts := learnerTestOptions()
	opts.Retrieval.(*fakeLearnerRetrievalRepo).list = []storage.RetrievalItem{
		{SubjectType: "concept", Subject: "te-form", DueAt: time.Now(), Interval: 24 * time.Hour},
	}

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	body := rec.Body.String()

	// Every section is a <details>, so none of this needs JavaScript.
	if strings.Count(body, `class="panel"`) == 0 && strings.Count(body, `class="panel" open`) == 0 {
		t.Fatalf("no panels rendered:\n%s", body)
	}
	// 優先項目 and 復習キュー open; the diagnostics do not.
	if !strings.Contains(body, `<details class="panel" open>`) {
		t.Error("nothing is open; the page opens on a wall of closed sections")
	}
	if !strings.Contains(body, `<details class="panel">`) {
		t.Error("nothing is closed; every section is expanded as before")
	}
}

// The planner's rationale is scoring diagnostics — the widest thing on
// the page and not what a learner asked for. It stays available, behind
// the disclosure, rather than on the line.
func TestThePlannerRationaleIsBehindTheDisclosureNotOnTheLine(t *testing.T) {
	opts := learnerTestOptions()
	opts.Priorities.(*fakeLearnerPriorityRepo).top = []storage.Priority{
		{SubjectType: "concept", Subject: "te-form", Score: 72,
			Reason: "recurring weakness: 23 occurrences in 30d (persistence 3.00)"},
	}

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	body := rec.Body.String()

	// Present…
	if !strings.Contains(body, "persistence 3.00") {
		t.Errorf("the rationale was dropped entirely:\n%s", body)
	}
	// …but inside the detail block, not the summary line.
	// Locating the line rather than assuming it: a marker that has
	// stopped matching slices an empty string, and every assertion below
	// then passes on nothing. That is exactly how the /vocabulary
	// pagination test spent two redesigns green and blind.
	start := strings.Index(body, `class="list__line"`)
	if start < 0 {
		t.Fatalf("no rows rendered — the marker no longer matches the markup:\n%s", body)
	}
	end := strings.Index(body[start:], "</summary>")
	if end < 0 {
		t.Fatalf("the row has no summary to close:\n%s", body[start:])
	}
	line := body[start : start+end]
	if !strings.Contains(line, "te-form") {
		t.Fatalf("sliced the wrong element; it does not hold the subject:\n%s", line)
	}
	if strings.Contains(line, "persistence") {
		t.Errorf("the rationale is on the summary line:\n%s", line)
	}
}

// 観察 is unbounded — every subject the model ever formed a view about.
// A list that quietly stops is one nobody knows is incomplete, so the
// remainder is counted.
func TestLearnerPageCountsTheObservationsItDoesNotShow(t *testing.T) {
	opts := learnerTestOptions()
	var many []learnermodel.Observation
	for i := 0; i < learnerObservationLimit+5; i++ {
		many = append(many, learnermodel.Observation{
			SubjectType: learnermodel.SubjectConcept,
			Subject:     "concept-" + strconv.Itoa(i),
			Kind:        learnermodel.KindWeakness,
			UpdatedAt:   time.Now(),
		})
	}
	opts.Observations.(*fakeObservationRepo).list = many

	rec := httptest.NewRecorder()
	NewServer(opts).HandlerForTest().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/learner", nil))
	body := rec.Body.String()

	if strings.Count(body, `class="insight"`) > learnerObservationLimit+len(opts.Priorities.(*fakeLearnerPriorityRepo).top)+10 {
		t.Error("the observation list was not capped")
	}
	if !strings.Contains(body, "ほか5件") {
		t.Errorf("the hidden remainder was not reported:\n%s", body)
	}
}
