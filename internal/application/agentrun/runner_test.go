package agentrun_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/adapters/fakeai" //nolint:depguard // fakeai is a port-shaped test double (implements ai.ToolCaller) injected via agentrun.NewRunner(ai.ToolCaller, ...); PRD §75 Rule 3 forbids application depending on real adapters, not fakes constructed in tests
	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// fakeRepo is an in-memory storage.AgentRunRepository double: it
// records every Start/Finish/RecordToolCall call verbatim so tests can
// assert on exactly what the loop persisted, without a real database.
// recordErrs lets a test make one specific RecordToolCall call
// (indexed 0-based across the whole run) fail, to pin the "trace
// persistence failure aborts the run" behaviour.
type fakeRepo struct {
	started    []storage.AgentRun
	finished   []finishCall
	toolCalls  []storage.ToolCall
	recordErrs map[int]error
}

type finishCall struct {
	Identity learner.IdentityID
	RunID    string
	Status   string
	Err      string
	Output   string
	Turns    int
}

func (f *fakeRepo) Start(_ context.Context, run storage.AgentRun) error {
	f.started = append(f.started, run)
	return nil
}

func (f *fakeRepo) Finish(_ context.Context, identity learner.IdentityID, runID, status, errMsg, output string, turns int, _ time.Time) error {
	f.finished = append(f.finished, finishCall{Identity: identity, RunID: runID, Status: status, Err: errMsg, Output: output, Turns: turns})
	return nil
}

func (f *fakeRepo) RecordToolCall(_ context.Context, _ learner.IdentityID, c storage.ToolCall) error {
	idx := len(f.toolCalls)
	f.toolCalls = append(f.toolCalls, c)
	if err, ok := f.recordErrs[idx]; ok {
		return err
	}
	return nil
}

func (f *fakeRepo) List(context.Context, learner.IdentityID, int) ([]storage.AgentRun, error) {
	return nil, nil
}

func (f *fakeRepo) Get(context.Context, learner.IdentityID, string) (storage.AgentRun, []storage.ToolCall, error) {
	return storage.AgentRun{}, nil, nil
}

// scriptedCaller returns each of responses in order, one per
// CallWithTools call, ignoring the request entirely — full control
// over the conversation shape for tests that need something fakeai's
// own fixed get_learning_priorities script doesn't cover (a refused
// tool, max-turns exhaustion, a handler error).
type scriptedCaller struct {
	responses []ai.ToolResponse
	calls     int
}

func (s *scriptedCaller) CallWithTools(context.Context, ai.ToolRequest) (ai.ToolResponse, error) {
	if s.calls >= len(s.responses) {
		return ai.ToolResponse{}, fmt.Errorf("scriptedCaller: no response scripted for call %d", s.calls+1)
	}
	resp := s.responses[s.calls]
	s.calls++
	return resp, nil
}

// erroringCaller always fails, to pin Run's CallWithTools-error path.
type erroringCaller struct{}

func (erroringCaller) CallWithTools(context.Context, ai.ToolRequest) (ai.ToolResponse, error) {
	return ai.ToolResponse{}, errors.New("provider unreachable")
}

func newTestTool(name string, isErr bool, content string) tools.Tool {
	return tools.Tool{
		Def: ai.ToolDef{Name: name, Description: "test tool", Schema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, learner.IdentityID, *session.ID, json.RawMessage) (string, error) {
			if isErr {
				return "", errors.New(content)
			}
			return content, nil
		},
	}
}

// TestRunTwoTurnScriptCompletesWithOneToolCall drives the exact
// two-turn script fakeai.CallWithTools implements (see
// internal/adapters/fakeai/toolcall_test.go): the model calls
// get_learning_priorities, then answers in prose naming the priority
// the tool returned. This pins the task brief's Step 1 happy path: one
// completed agent_runs row with turns=2, one tool_calls row.
func TestRunTwoTurnScriptCompletesWithOneToolCall(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("get_learning_priorities", false, `[{"subject_type":"concept","subject":"i-adjective-past","score":5,"reason":"recurring"}]`))
	reg.Allow("teacher", "get_learning_priorities")

	repo := &fakeRepo{}
	runner := agentrun.NewRunner(fakeai.New(), reg, repo, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:         "teacher",
		PromptName:    "teacher.agentic",
		PromptVersion: "v1",
		System:        "You are an agentic Japanese writing teacher.",
		Messages:      []ai.ToolMessage{{Role: "user", Text: "What should I focus on next?"}},
		Identity:      "mikey",
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Turns != 2 {
		t.Errorf("Turns = %d, want 2", out.Turns)
	}
	if !strings.Contains(out.Text, "i-adjective-past") {
		t.Errorf("Text = %q, want it to mention i-adjective-past", out.Text)
	}
	if out.RunID == "" {
		t.Error("RunID is empty")
	}

	if len(repo.started) != 1 || repo.started[0].Status != "running" || repo.started[0].Agent != "teacher" {
		t.Fatalf("started = %+v, want exactly one running teacher row", repo.started)
	}
	if repo.started[0].System != "You are an agentic Japanese writing teacher." || repo.started[0].Input != "What should I focus on next?" {
		t.Errorf("started[0] System/Input = %q/%q, want the run's opening system prompt and user turn", repo.started[0].System, repo.started[0].Input)
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != "completed" || repo.finished[0].Turns != 2 {
		t.Fatalf("finished = %+v, want one completed row with turns=2", repo.finished)
	}
	if !strings.Contains(repo.finished[0].Output, "i-adjective-past") {
		t.Errorf("finished Output = %q, want it to carry the model's final turn text", repo.finished[0].Output)
	}
	if len(repo.toolCalls) != 1 {
		t.Fatalf("toolCalls = %+v, want exactly one row", repo.toolCalls)
	}
	tc := repo.toolCalls[0]
	if tc.ToolName != "get_learning_priorities" || tc.IsError || tc.AgentRunID != out.RunID {
		t.Errorf("toolCalls[0] = %+v, want a non-error get_learning_priorities call against %q", tc, out.RunID)
	}
}

// TestRunRefusedToolStillRecordsTraceRowAndContinues pins the
// critical requirement carried from Task 1's review: a tool the model
// calls but the agent isn't Allow()ed to use must still produce a
// tool_calls row (IsError=true), and the loop must continue rather
// than aborting — the model gets to see the refusal and react.
func TestRunRefusedToolStillRecordsTraceRowAndContinues(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("forbidden_tool", false, "should never run"))
	// Deliberately NOT Allow()ed for "teacher".

	caller := &scriptedCaller{responses: []ai.ToolResponse{
		{Turn: ai.ToolTurn{Invocations: []ai.ToolInvocation{{ID: "1", Name: "forbidden_tool", Arguments: json.RawMessage(`{}`)}}}},
		{Turn: ai.ToolTurn{Text: "noted, moving on"}},
	}}
	repo := &fakeRepo{}
	runner := agentrun.NewRunner(caller, reg, repo, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Turns != 2 {
		t.Errorf("Turns = %d, want 2", out.Turns)
	}
	if len(repo.toolCalls) != 1 || !repo.toolCalls[0].IsError {
		t.Fatalf("toolCalls = %+v, want exactly one IsError=true row for the refused tool", repo.toolCalls)
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != "completed" {
		t.Fatalf("finished = %+v, want a completed run — a refusal must not fail the whole run", repo.finished)
	}
}

// TestRunToolHandlerErrorDoesNotAbortRun pins the brief's "a tool
// handler error doesn't abort the run" requirement: the handler's
// error becomes an IsError tool result the model can react to, same
// as a refusal, and the run still completes normally.
func TestRunToolHandlerErrorDoesNotAbortRun(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("flaky_tool", true, "boom"))
	reg.Allow("teacher", "flaky_tool")

	caller := &scriptedCaller{responses: []ai.ToolResponse{
		{Turn: ai.ToolTurn{Invocations: []ai.ToolInvocation{{ID: "1", Name: "flaky_tool", Arguments: json.RawMessage(`{}`)}}}},
		{Turn: ai.ToolTurn{Text: "handled the error, here is my answer"}},
	}}
	repo := &fakeRepo{}
	runner := agentrun.NewRunner(caller, reg, repo, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(repo.toolCalls) != 1 || !repo.toolCalls[0].IsError || repo.toolCalls[0].Result != "boom" {
		t.Fatalf("toolCalls = %+v, want one IsError row carrying the handler's error message", repo.toolCalls)
	}
	if out.Turns != 2 || out.Text != "handled the error, here is my answer" {
		t.Errorf("out = %+v, want the run to complete on turn 2 with the model's answer", out)
	}
}

// TestRunMaxTurnsExhaustionFailsRun pins the brief's "hitting the cap
// is a failed run with a clear error, not a silent truncation":
// MaxTurns=1 against a model that always wants another tool call must
// fail, not quietly return whatever partial text it has.
func TestRunMaxTurnsExhaustionFailsRun(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("allowed_tool", false, "ok"))
	reg.Allow("teacher", "allowed_tool")

	caller := &scriptedCaller{responses: []ai.ToolResponse{
		{Turn: ai.ToolTurn{Invocations: []ai.ToolInvocation{{ID: "1", Name: "allowed_tool", Arguments: json.RawMessage(`{}`)}}}},
	}}
	repo := &fakeRepo{}
	runner := agentrun.NewRunner(caller, reg, repo, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
		MaxTurns: 1,
	})
	if err == nil {
		t.Fatal("Run returned nil error, want a MaxTurns-exhaustion error")
	}
	if out.RunID == "" {
		t.Error("RunID is empty even on failure, want it set so the trace can still be looked up")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != "failed" || repo.finished[0].Turns != 1 {
		t.Fatalf("finished = %+v, want one failed row with turns=1", repo.finished)
	}
	if repo.finished[0].Err == "" {
		t.Error("finished error message is empty, want a clear MaxTurns-exhaustion message")
	}
	// The tool call the model DID make on its last turn is still
	// recorded — the trace shows exactly what happened before the cap
	// was hit.
	if len(repo.toolCalls) != 1 {
		t.Fatalf("toolCalls = %+v, want the last turn's tool call still recorded before failing", repo.toolCalls)
	}
}

// TestRunRecordToolCallFailureFailsRun pins the hard-fail policy for
// trace-persistence failures (see the Run doc comment): if a
// tool_calls row can't be written, the run must not silently continue
// as if the audit trail were intact.
func TestRunRecordToolCallFailureFailsRun(t *testing.T) {
	reg := tools.NewRegistry()
	reg.Register(newTestTool("allowed_tool", false, "ok"))
	reg.Allow("teacher", "allowed_tool")

	caller := &scriptedCaller{responses: []ai.ToolResponse{
		{Turn: ai.ToolTurn{Invocations: []ai.ToolInvocation{{ID: "1", Name: "allowed_tool", Arguments: json.RawMessage(`{}`)}}}},
		{Turn: ai.ToolTurn{Text: "done"}},
	}}
	repo := &fakeRepo{recordErrs: map[int]error{0: errors.New("db down")}}
	runner := agentrun.NewRunner(caller, reg, repo, time.Now)

	_, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("Run returned nil error, want the RecordToolCall failure surfaced")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != "failed" {
		t.Fatalf("finished = %+v, want a failed row when the trace itself can't be persisted", repo.finished)
	}
}

// TestRunCallWithToolsErrorFailsRun pins the provider-error path: a
// transport/network failure from the model itself must fail the run
// cleanly, with a trace row explaining why.
func TestRunCallWithToolsErrorFailsRun(t *testing.T) {
	reg := tools.NewRegistry()
	repo := &fakeRepo{}
	runner := agentrun.NewRunner(erroringCaller{}, reg, repo, time.Now)

	_, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err == nil {
		t.Fatal("Run returned nil error, want the provider error surfaced")
	}
	if len(repo.finished) != 1 || repo.finished[0].Status != "failed" {
		t.Fatalf("finished = %+v, want a failed row", repo.finished)
	}
	if repo.finished[0].Turns != 0 {
		t.Errorf("finished Turns = %d, want 0 — the first attempt never got a model response, so it isn't a completed turn", repo.finished[0].Turns)
	}
}

// TestRunDefaultsMaxTurnsToEight pins the brief's documented default:
// RunInput.MaxTurns left at zero must behave as 8, not 0 (which would
// make every run fail immediately without ever calling the model).
func TestRunDefaultsMaxTurnsToEight(t *testing.T) {
	reg := tools.NewRegistry()
	repo := &fakeRepo{}
	// A caller that answers immediately with no invocations: if
	// MaxTurns silently defaulted to something less than 1 the loop
	// body would never execute and Run would return the "exhausted max
	// turns unexpectedly" fallback error instead of completing normally.
	caller := &scriptedCaller{responses: []ai.ToolResponse{{Turn: ai.ToolTurn{Text: "done"}}}}
	runner := agentrun.NewRunner(caller, reg, repo, time.Now)

	out, err := runner.Run(context.Background(), agentrun.RunInput{
		Agent:    "teacher",
		Identity: "mikey",
		Messages: []ai.ToolMessage{{Role: "user", Text: "hi"}},
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if out.Turns != 1 {
		t.Errorf("Turns = %d, want 1", out.Turns)
	}
}
