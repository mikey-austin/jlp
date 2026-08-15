package a2a_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/a2a"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double (implements ai.ToolCaller), injected via agentrun.NewRunner the same way internal/application/agentrun's own tests do
	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// fakeRepo is a minimal in-memory storage.AgentRunRepository double —
// this package's own copy of application/agentrun's own test fixture
// of the same name/shape (see runner_test.go), needed here too since
// building a real *agentrun.Runner (this adapter's one collaborator
// besides *tools.Registry) requires one.
type fakeRepo struct{}

func (fakeRepo) Start(context.Context, storage.AgentRun) error { return nil }
func (fakeRepo) Finish(context.Context, learner.IdentityID, string, string, string, string, int, time.Time) error {
	return nil
}
func (fakeRepo) RecordToolCall(context.Context, learner.IdentityID, storage.ToolCall) error {
	return nil
}
func (fakeRepo) RecordTurn(context.Context, learner.IdentityID, storage.AgentTurn) error { return nil }
func (fakeRepo) List(context.Context, learner.IdentityID, int) ([]storage.AgentRun, error) {
	return nil, nil
}
func (fakeRepo) Get(context.Context, learner.IdentityID, string) (storage.AgentRun, []storage.ToolCall, []storage.AgentTurn, error) {
	return storage.AgentRun{}, nil, nil, nil
}

// recordingTool is a tools.Tool whose handler records the identity
// it was actually invoked with — this test's way of pinning "a forged
// identity in the task body is never what the tool sees" end to end
// (mirroring internal/tools's own
// TestInvokeIgnoresForgedIdentityInArguments), through the FULL HTTP
// stack this time: httptest request → handleCreateTask →
// agentrun.Runner.Run → tools.Registry.Invoke → this handler.
type recordingTool struct {
	name string
	seen *[]learner.IdentityID
}

func (rt recordingTool) register(reg *tools.Registry) {
	reg.Register(tools.Tool{
		Def: ai.ToolDef{Name: rt.name, Description: "test tool", Schema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(_ context.Context, identity learner.IdentityID, _ *session.ID, _ json.RawMessage) (string, error) {
			*rt.seen = append(*rt.seen, identity)
			return `[{"subject_type":"concept","subject":"i-adjective-past","score":5,"reason":"recurring"}]`, nil
		},
	})
}

// newTestServer wires an a2a.Server over a real *agentrun.Runner
// (fakeai.New() as the ai.ToolCaller, fakeRepo{} as the trace
// repository) and a *tools.Registry that Allow()s ONLY "teacher" to
// call "get_learning_priorities" — the exact name fakeai's
// CallWithTools script always invokes first (see
// internal/adapters/fakeai's own doc comment), and the exact
// allowlist cmd/jlp/main.go's real wiring grants "teacher" — so
// review_writing's underlying agent gets a real (non-error) tool
// result and analyse_learner/plan_lesson's ("summary"/"lesson", not
// Allow()ed here either, matching main.go's current wiring) get a
// refusal, exactly like production.
func newTestServer(t *testing.T) (*a2a.Server, *[]learner.IdentityID) {
	t.Helper()
	reg := tools.NewRegistry()
	var seen []learner.IdentityID
	recordingTool{name: "get_learning_priorities", seen: &seen}.register(reg)
	reg.Allow("teacher", "get_learning_priorities")

	runner := agentrun.NewRunner(fakeai.New(), reg, fakeRepo{}, time.Now)
	return a2a.New(runner, reg, config.A2A{Enabled: true, Path: "/a2a"}), &seen
}

func doRequest(t *testing.T, h http.Handler, method, path string, identity learner.IdentityID, body string) *httptest.ResponseRecorder {
	t.Helper()
	var req *http.Request
	if body != "" {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	} else {
		req = httptest.NewRequest(method, path, nil)
	}
	if identity != "" {
		req = req.WithContext(a2a.WithIdentity(req.Context(), identity))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestAgentCardShape pins the card's top-level shape: three skills, in
// the documented order, capabilities both reported false (synchronous
// execution only — see task.go's handleCreateTask doc comment), and —
// the Rule 13 transparency this adapter exists to demonstrate — each
// skill's "tools" field read LIVE off the same Registry this test
// configured above: review_writing ("teacher") sees exactly the one
// tool Allow()ed, analyse_learner/plan_lesson ("summary"/"lesson", not
// Allow()ed anything here) see none.
func TestAgentCardShape(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodGet, "/.well-known/agent-card.json", "", "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var card a2a.AgentCard
	if err := json.Unmarshal(rec.Body.Bytes(), &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if card.Name == "" {
		t.Error("card Name is empty")
	}
	if len(card.Skills) != 3 {
		t.Fatalf("len(Skills) = %d, want 3", len(card.Skills))
	}
	wantIDs := []string{"review_writing", "analyse_learner", "plan_lesson"}
	for i, want := range wantIDs {
		if card.Skills[i].ID != want {
			t.Errorf("Skills[%d].ID = %q, want %q", i, card.Skills[i].ID, want)
		}
	}
	if card.Capabilities.Streaming || card.Capabilities.PushNotifications {
		t.Errorf("Capabilities = %+v, want both false (synchronous adapter)", card.Capabilities)
	}

	var reviewWriting, analyseLearner a2a.SkillCard
	for _, sk := range card.Skills {
		switch sk.ID {
		case "review_writing":
			reviewWriting = sk
		case "analyse_learner":
			analyseLearner = sk
		}
	}
	if len(reviewWriting.Tools) != 1 || reviewWriting.Tools[0] != "get_learning_priorities" {
		t.Errorf("review_writing Tools = %v, want exactly [get_learning_priorities] (this test's own Registry.Allow)", reviewWriting.Tools)
	}
	if len(analyseLearner.Tools) != 0 {
		t.Errorf("analyse_learner Tools = %v, want empty (no Allow() configured for \"summary\")", analyseLearner.Tools)
	}
}

// TestCreateTaskReviewWritingReturnsCorrections drives review_writing
// end to end through the real fakeai two-turn script (tool call, then
// prose naming the tool's returned subject) — the Step 1 happy path:
// a completed task whose output actually reflects the underlying
// agent-run's investigation, not a stub.
func TestCreateTaskReviewWritingReturnsCorrections(t *testing.T) {
	srv, _ := newTestServer(t)
	body := `{"skill":"review_writing","input":"友達と映画を見ました。とても面白いでした。"}`
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "mikey", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if resp.Status != "completed" {
		t.Fatalf("Status = %q, want completed: %+v", resp.Status, resp)
	}
	if resp.TaskID == "" {
		t.Error("TaskID is empty")
	}
	if !strings.Contains(resp.Output, "i-adjective-past") {
		t.Errorf("Output = %q, want it to mention i-adjective-past (the fake tool's returned priority)", resp.Output)
	}
}

// TestCreateTaskUnknownSkillReturns400 pins the Step 1 "unknown skill
// → 400" case.
func TestCreateTaskUnknownSkillReturns400(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "mikey", `{"skill":"not_a_real_skill","input":"hello"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTaskMissingInputReturns400(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "mikey", `{"skill":"review_writing","input":"   "}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestCreateTaskMalformedJSONReturns400(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "mikey", `{not json`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateTaskRequiresAuthenticatedIdentity pins that a request
// reaching this adapter with NO context identity at all (should never
// happen in production — Routes() is only ever mounted inside
// server.go's authenticated group — but this adapter must not assume
// its caller got that right) is rejected rather than silently running
// as some zero-value identity.
func TestCreateTaskRequiresAuthenticatedIdentity(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "", `{"skill":"review_writing","input":"hello"}`)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401: %s", rec.Code, rec.Body.String())
	}
}

// TestCreateTaskIgnoresForgedIdentityInBody is this task's required
// pin (mirroring internal/tools's
// TestInvokeIgnoresForgedIdentityInArguments): a task payload that
// carries a JSON "identity" field naming someone else must have
// ZERO effect on which identity the run — and every tool call inside
// it — actually executes as. The request's real, context-supplied
// identity ("real-caller", set the same way
// internal/adapters/http/server.go's withA2AIdentity sets it from
// RequireIdentity's own resolution) is the only one that reaches the
// recordingTool handler, proven by asserting on *seen below, not by
// inspecting the response (which never echoes an identity back at
// all).
func TestCreateTaskIgnoresForgedIdentityInBody(t *testing.T) {
	srv, seen := newTestServer(t)
	body := `{"skill":"review_writing","input":"友達と映画を見ました。","identity":"someone-else"}`
	rec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "real-caller", body)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(*seen) != 1 {
		t.Fatalf("tool invoked %d times, want exactly 1", len(*seen))
	}
	if (*seen)[0] != "real-caller" {
		t.Fatalf("tool saw identity %q, want %q (the forged \"identity\" field must be ignored entirely)", (*seen)[0], "real-caller")
	}
}

// TestGetTaskRoundTripsCreatedTask pins GET {path}/tasks/{task_id}
// reading back exactly what POST already computed (see
// handleCreateTask's doc comment on synchronous execution — GET never
// finds a "running" task, only ever the already-final result).
func TestGetTaskRoundTripsCreatedTask(t *testing.T) {
	srv, _ := newTestServer(t)
	createRec := doRequest(t, srv.Routes(), http.MethodPost, "/tasks", "mikey", `{"skill":"review_writing","input":"hello"}`)
	var created struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal create response: %v", err)
	}

	getRec := doRequest(t, srv.Routes(), http.MethodGet, "/tasks/"+created.TaskID, "", "")
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200: %s", getRec.Code, getRec.Body.String())
	}
	var got struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(getRec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal get response: %v", err)
	}
	if got.TaskID != created.TaskID || got.Status != created.Status || got.Output != created.Output {
		t.Errorf("GET %+v, want it to match the POST response %+v exactly", got, created)
	}
}

func TestGetTaskUnknownReturns404(t *testing.T) {
	srv, _ := newTestServer(t)
	rec := doRequest(t, srv.Routes(), http.MethodGet, "/tasks/does-not-exist", "", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}
