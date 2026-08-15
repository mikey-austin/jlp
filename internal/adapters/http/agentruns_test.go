package httpx

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// fakeAgentRunRepo is an in-memory storage.AgentRunRepository double
// for HTTP-layer tests, mirroring the real postgres adapter's
// identity-scoped-via-join contract (a runID that exists but belongs
// to a different identity misses with storage.ErrNotFound, same as
// every other *Repo double in this package).
type fakeAgentRunRepo struct {
	runs  map[string]storage.AgentRun
	calls map[string][]storage.ToolCall
}

func newFakeAgentRunRepo() *fakeAgentRunRepo {
	return &fakeAgentRunRepo{runs: map[string]storage.AgentRun{}, calls: map[string][]storage.ToolCall{}}
}

func (f *fakeAgentRunRepo) Start(_ context.Context, run storage.AgentRun) error {
	f.runs[run.ID] = run
	return nil
}

func (f *fakeAgentRunRepo) Finish(_ context.Context, identity learner.IdentityID, runID, status, errMsg, output string, turns int, endedAt time.Time) error {
	run, ok := f.runs[runID]
	if !ok || run.IdentityID != identity {
		return storage.ErrNotFound
	}
	run.Status, run.Error, run.Output, run.Turns, run.EndedAt = status, errMsg, output, turns, endedAt
	f.runs[runID] = run
	return nil
}

func (f *fakeAgentRunRepo) RecordToolCall(_ context.Context, identity learner.IdentityID, c storage.ToolCall) error {
	run, ok := f.runs[c.AgentRunID]
	if !ok || run.IdentityID != identity {
		return storage.ErrNotFound
	}
	f.calls[c.AgentRunID] = append(f.calls[c.AgentRunID], c)
	return nil
}

func (f *fakeAgentRunRepo) List(_ context.Context, identity learner.IdentityID, limit int) ([]storage.AgentRun, error) {
	var out []storage.AgentRun
	for _, run := range f.runs {
		if run.IdentityID == identity {
			out = append(out, run)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeAgentRunRepo) Get(_ context.Context, identity learner.IdentityID, runID string) (storage.AgentRun, []storage.ToolCall, error) {
	run, ok := f.runs[runID]
	if !ok || run.IdentityID != identity {
		return storage.AgentRun{}, nil, storage.ErrNotFound
	}
	return run, f.calls[runID], nil
}

func agentRunsTestServer(t *testing.T) (h http.Handler, repo *fakeAgentRunRepo) {
	t.Helper()
	opts := testOptions()
	repo = newFakeAgentRunRepo()
	opts.AgentRuns = repo
	srv := NewServer(opts)
	return srv.HandlerForTest(), repo
}

// TestAgentRunsListRendersRuns pins the brief's "list renders"
// requirement: a run started for the authenticated identity ("dev",
// per testOptions) shows up in the /ai/agents table with a link to its
// detail page.
func TestAgentRunsListRendersRuns(t *testing.T) {
	h, repo := agentRunsTestServer(t)
	now := time.Now().UTC()
	if err := repo.Start(context.Background(), storage.AgentRun{
		ID: "run-1", IdentityID: "dev", Agent: "teacher",
		PromptName: "teacher.agentic", PromptVersion: "v1",
		Status: "running", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Finish(context.Background(), "dev", "run-1", "completed", "", "Focus on i-adjective-past.", 2, now.Add(3*time.Second)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/agents", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "teacher") {
		t.Error("list page missing the run's agent name")
	}
	if !strings.Contains(body, "/ai/agents/run-1") {
		t.Error("list page missing a link to the run's detail page")
	}
	if !strings.Contains(body, "completed") {
		t.Error("list page missing the run's status")
	}
}

// TestAgentRunsDetailRendersToolCallAndOutput pins the brief's "detail
// renders a tool call" requirement, plus the full PRD §50 trace shape:
// input context, the tool call's arguments/result, and the final
// output.
func TestAgentRunsDetailRendersToolCallAndOutput(t *testing.T) {
	h, repo := agentRunsTestServer(t)
	now := time.Now().UTC()
	if err := repo.Start(context.Background(), storage.AgentRun{
		ID: "run-2", IdentityID: "dev", Agent: "teacher",
		PromptName: "teacher.agentic", PromptVersion: "v1",
		Status: "running", StartedAt: now,
		System: "You are an agentic Japanese writing teacher.",
		Input:  "What should I focus on next?",
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordToolCall(context.Background(), "dev", storage.ToolCall{
		ID: "call-1", AgentRunID: "run-2", ToolName: "get_learning_priorities",
		Arguments: `{"limit":5}`, Result: `[{"subject":"i-adjective-past"}]`,
		IsError: false, DurationMS: 12, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Finish(context.Background(), "dev", "run-2", "completed", "", "Focus on i-adjective-past based on recent priorities.", 2, now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/agents/run-2", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		"You are an agentic Japanese writing teacher.",          // input context: System
		"What should I focus on next?",                          // input context: Input
		"get_learning_priorities",                               // tool call name
		`{&#34;limit&#34;:5}`,                                   // tool call arguments, HTML-escaped
		"i-adjective-past",                                      // tool call result content
		"Focus on i-adjective-past based on recent priorities.", // final output
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail page missing %q\nbody:\n%s", want, body)
		}
	}
}

// TestAgentRunsDetailEscapesToolCallArguments pins the security
// requirement carried in the task's design guidance: model-generated
// tool arguments/results must render escaped, never as raw HTML — a
// tool result containing "<script>" must show up as inert escaped
// text, not an executable tag.
func TestAgentRunsDetailEscapesToolCallArguments(t *testing.T) {
	h, repo := agentRunsTestServer(t)
	now := time.Now().UTC()
	if err := repo.Start(context.Background(), storage.AgentRun{
		ID: "run-3", IdentityID: "dev", Agent: "teacher",
		PromptName: "teacher.agentic", PromptVersion: "v1",
		Status: "running", StartedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := repo.RecordToolCall(context.Background(), "dev", storage.ToolCall{
		ID: "call-2", AgentRunID: "run-3", ToolName: "get_recent_writing",
		Arguments: `{}`, Result: `<script>alert(1)</script>`,
		IsError: true, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/agents/run-3", nil))
	body := rec.Body.String()
	if strings.Contains(body, "<script>alert(1)</script>") {
		t.Fatal("detail page rendered a tool result as raw HTML — must be escaped")
	}
	if !strings.Contains(body, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Errorf("detail page missing the escaped tool result\nbody:\n%s", body)
	}
}

// TestAgentRunsDetailCrossIdentityMisses404 pins the task brief's
// "cross-identity detail → 404" requirement.
func TestAgentRunsDetailCrossIdentityMisses404(t *testing.T) {
	h, repo := agentRunsTestServer(t)
	if err := repo.Start(context.Background(), storage.AgentRun{
		ID: "run-other", IdentityID: "someone-else", Agent: "teacher",
		PromptName: "teacher.agentic", PromptVersion: "v1",
		Status: "running", StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/agents/run-other", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}

// TestAgentRunsDetailUnknownIDReturns404 mirrors the not-found
// contract every other detail page in this package uses.
func TestAgentRunsDetailUnknownIDReturns404(t *testing.T) {
	h, _ := agentRunsTestServer(t)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/ai/agents/does-not-exist", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body=%s", rec.Code, rec.Body.String())
	}
}
