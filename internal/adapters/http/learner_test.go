package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
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
