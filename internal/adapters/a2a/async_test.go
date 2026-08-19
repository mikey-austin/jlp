package a2a

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// blockingRunner holds a run open until released, so a test can observe
// the state a caller sees WHILE the work is still going — the whole
// point of returning early.
type blockingRunner struct {
	started  chan struct{}
	once     sync.Once // the cap test runs several at once; only the first signals
	release  chan struct{}
	mu       sync.Mutex
	sawCtxOK bool
}

func (r *blockingRunner) Run(ctx context.Context, _ agentrun.RunInput) (agentrun.RunOutput, error) {
	r.once.Do(func() { close(r.started) })
	<-r.release
	// Whether the context is still live here is the question that
	// matters: the HTTP request's context is cancelled the moment
	// SendMessage responds, so a run started from it would die.
	r.mu.Lock()
	r.sawCtxOK = ctx.Err() == nil
	r.mu.Unlock()
	return agentrun.RunOutput{RunID: "run-1", Text: "done at last"}, nil
}

func asyncServer(t *testing.T, runner agentRunner) *Server {
	t.Helper()
	return New(runner, tools.NewRegistry(), config.A2A{Enabled: true, Path: "/a2a"})
}

func sendAsync(t *testing.T, s *Server, ctx context.Context) Task {
	t.Helper()
	params, err := json.Marshal(map[string]any{
		"message":       map[string]any{"parts": []any{map[string]any{"text": "分析して"}}},
		"configuration": map[string]any{"returnImmediately": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	res, rpcErr := s.handleSendMessage(ctx, "mikey", params)
	if rpcErr != nil {
		t.Fatalf("SendMessage: %+v", rpcErr)
	}
	out, ok := res.(sendMessageResult)
	if !ok || out.Task == nil {
		t.Fatalf("result is %T, want a task", res)
	}
	return *out.Task
}

// The caller gets a task id back while the run is still going. That is
// the whole feature: a coordinate run takes minutes, and holding a
// connection open for it failed at the server's write timeout and then
// again at the client's header timeout.
func TestReturnImmediatelyRespondsWhileTheRunIsStillGoing(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	task := sendAsync(t, s, context.Background())
	if task.Status.State != taskStateWorking {
		t.Fatalf("state = %q, want %q", task.Status.State, taskStateWorking)
	}
	if task.ID == "" {
		t.Fatal("no task id, so the caller has nothing to poll for")
	}

	// Still working, because we have not released the run.
	<-runner.started
	if got := pollState(t, s, task.ID); got != taskStateWorking {
		t.Errorf("GetTask reports %q while the run is in flight, want %q", got, taskStateWorking)
	}

	close(runner.release)
	if got := waitForTerminal(t, s, task.ID); got != taskStateCompleted {
		t.Errorf("final state = %q, want %q", got, taskStateCompleted)
	}
}

// The finished answer must land under the SAME task id the caller was
// given, or polling finds a task that never changes.
func TestTheFinishedAnswerReplacesTheWorkingTask(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	task := sendAsync(t, s, context.Background())
	<-runner.started
	close(runner.release)
	waitForTerminal(t, s, task.ID)

	rec, rpcErr := s.lookup("mikey", task.ID)
	if rpcErr != nil {
		t.Fatalf("lookup: %+v", rpcErr)
	}
	if len(rec.Task.Artifacts) == 0 {
		t.Fatal("the completed task carries no artifact")
	}
	got := rec.Task.Artifacts[0].Parts[0].Text
	if got == nil || *got != "done at last" {
		t.Errorf("artifact text = %v, want the run's answer", got)
	}
}

// The request's context dies when SendMessage responds. A background run
// started from it would be cancelled immediately — the bug this guards.
func TestTheBackgroundRunSurvivesTheRequestContext(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	ctx, cancel := context.WithCancel(context.Background())
	task := sendAsync(t, s, ctx)
	<-runner.started
	cancel() // exactly what the HTTP server does once the response is written
	close(runner.release)
	waitForTerminal(t, s, task.ID)

	runner.mu.Lock()
	defer runner.mu.Unlock()
	if !runner.sawCtxOK {
		t.Error("the background run saw a cancelled context, so cancelling the request would kill work already promised")
	}
}

// A blocking caller cannot start a second run while it waits.
// returnImmediately removes that brake, so something else has to.
func TestBackgroundRunsAreCapped(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	var lastState string
	for i := 0; i < maxConcurrentAsyncRuns+1; i++ {
		lastState = sendAsync(t, s, context.Background()).Status.State
	}
	if lastState != taskStateFailed {
		t.Errorf("run %d past the cap returned %q, want %q — nothing bounds concurrent model spend",
			maxConcurrentAsyncRuns+1, lastState, taskStateFailed)
	}
	close(runner.release)
}

// Cancel is only meaningful now that a caller can hold a task id while
// the work is still going. Before this, every task was already finished.
func TestCancelStopsABackgroundRun(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	task := sendAsync(t, s, context.Background())
	<-runner.started

	res, rpcErr := s.handleCancelTask("mikey", mustJSON(t, map[string]any{"id": task.ID}))
	if rpcErr != nil {
		t.Fatalf("CancelTask: %+v", rpcErr)
	}
	got, ok := res.(Task)
	if !ok {
		t.Fatalf("CancelTask returned %T, want a Task", res)
	}
	if got.Status.State != taskStateCanceled {
		t.Errorf("state = %q, want %q", got.Status.State, taskStateCanceled)
	}

	// The run's own error ("context canceled") must not overwrite the
	// state the user asked for.
	close(runner.release)
	time.Sleep(50 * time.Millisecond)
	if s := pollState(t, s, task.ID); s != taskStateCanceled {
		t.Errorf("after the run unwound, state = %q, want it to stay %q", s, taskStateCanceled)
	}
}

// A finished task has nothing to stop, and saying otherwise would tell a
// client it had halted work that in fact already completed.
func TestCancelRefusesAFinishedTask(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	close(runner.release) // never blocks: the run finishes immediately
	s := asyncServer(t, runner)

	task := sendAsync(t, s, context.Background())
	waitForTerminal(t, s, task.ID)

	_, rpcErr := s.handleCancelTask("mikey", mustJSON(t, map[string]any{"id": task.ID}))
	if rpcErr == nil {
		t.Fatal("CancelTask accepted a completed task")
	}
	if rpcErr.Code != errTaskNotCancelable {
		t.Errorf("code = %d, want %d (TASK_NOT_CANCELABLE)", rpcErr.Code, errTaskNotCancelable)
	}
}

// Cancelling someone else's task must answer exactly as GetTask does for
// a task you cannot read — "not cancelable" would confirm it exists.
func TestCancelDoesNotLeakAnotherIdentitysTask(t *testing.T) {
	runner := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	s := asyncServer(t, runner)

	task := sendAsync(t, s, context.Background())
	<-runner.started

	_, rpcErr := s.handleCancelTask("someone-else", mustJSON(t, map[string]any{"id": task.ID}))
	if rpcErr == nil {
		t.Fatal("CancelTask let one identity cancel another's task")
	}
	if rpcErr.Code == errTaskNotCancelable {
		t.Error("answered TASK_NOT_CANCELABLE, which confirms the task exists to someone who cannot read it")
	}

	close(runner.release)
	if got := waitForTerminal(t, s, task.ID); got != taskStateCompleted {
		t.Errorf("the owner's run ended %q; the refused cancel should not have touched it", got)
	}
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pollState(t *testing.T, s *Server, id string) string {
	t.Helper()
	rec, rpcErr := s.lookup("mikey", id)
	if rpcErr != nil {
		t.Fatalf("lookup %s: %+v", id, rpcErr)
	}
	return rec.Task.Status.State
}

func waitForTerminal(t *testing.T, s *Server, id string) string {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if got := pollState(t, s, id); got == taskStateCompleted || got == taskStateFailed {
			return got
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("task %s never reached a terminal state", id)
	return ""
}
