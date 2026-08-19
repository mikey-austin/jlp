package a2a

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
)

// Non-blocking SendMessage: return a task id now, finish the run in the
// background, let the caller poll GetTask.
//
// This is A2A's own answer to long work, and the reason the field was
// worth honouring rather than working around: a coordinate run is two
// full agent runs and takes minutes. Holding an HTTP connection open for
// that failed twice — the server's 60s WriteTimeout cut a completed run
// off from its caller, and raising it only moved the wall to the
// client's 300s header timeout. Both were the same mistake: pretending a
// long job is a short request.

const (
	// asyncRunTimeout bounds a background run. The request context is
	// gone by then, so without this a wedged provider would hold a
	// goroutine (and its model call) forever.
	//
	// Generous: a coordinate run legitimately takes minutes, and the
	// MaxTurns cap is the real bound on work. This is the backstop for a
	// provider that never answers at all.
	asyncRunTimeout = 15 * time.Minute

	// maxConcurrentAsyncRuns caps background runs in flight.
	//
	// A blocking request is self-limiting: the caller waits, so it cannot
	// start a second. returnImmediately removes that brake — a client in
	// a loop could otherwise start unbounded agent runs, each burning
	// real money. Refusing past the cap is a worse experience than
	// queueing and a much better one than an unbounded bill.
	maxConcurrentAsyncRuns = 4
)

// startAsync registers a task in TASK_STATE_WORKING, kicks off the run,
// and returns the task immediately.
func (s *Server) startAsync(
	ctx context.Context,
	identity learner.IdentityID,
	p sendMessageParams,
	skillID string,
	def skillDef,
	run agentrun.RunInput,
	contextID string,
) (any, *rpcError) {
	select {
	case s.asyncSlots <- struct{}{}:
	default:
		// A FAILED task rather than an RPC error, following the rule
		// jsonrpc.go states: the call succeeded, so the answer is a task
		// carrying the reason. It is also a shape every client already
		// handles, unlike a new error code.
		//
		// Refused rather than queued: a caller told "busy" can back off,
		// while a silent queue becomes a backlog of model calls nobody is
		// waiting for any more.
		busy := Task{
			ID:        uuid.NewString(),
			ContextID: contextID,
			Status: TaskStatus{
				State:     taskStateFailed,
				Timestamp: time.Now().UTC().Format(time.RFC3339),
			},
			Metadata: map[string]any{"skill": skillID},
		}
		busy.Status.Message = &Message{
			MessageID: uuid.NewString(),
			ContextID: contextID,
			TaskID:    busy.ID,
			Role:      roleAgent,
			Parts:     []Part{textPart("too many background tasks in flight; retry shortly")},
		}
		return sendMessageResult{Task: &busy}, nil
	}

	// The task id is minted here rather than taken from the run, because
	// the caller needs it before the run exists. The run's own id lives
	// in agent_runs and is reachable through /ai/agents.
	taskID := uuid.NewString()

	// context.WithoutCancel, not ctx: the HTTP request's context is
	// cancelled the moment this function returns its response, which
	// would kill the run we just promised to finish. Values (the run id
	// delegation reads, anything a middleware set) are preserved; only
	// the cancellation is dropped — and then a fresh deadline and cancel
	// are attached, the cancel being what CancelTask calls.
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), asyncRunTimeout)

	userMsg := p.Message
	if userMsg.MessageID == "" {
		userMsg.MessageID = uuid.NewString()
	}
	userMsg.Role = roleUser
	userMsg.TaskID = taskID
	userMsg.ContextID = contextID

	task := Task{
		ID:        taskID,
		ContextID: contextID,
		Status: TaskStatus{
			State:     taskStateWorking,
			Timestamp: time.Now().UTC().Format(time.RFC3339),
		},
		History:  []Message{userMsg},
		Metadata: map[string]any{"skill": skillID},
	}
	// cancel is stored with the task so CancelTask can reach it. This is
	// the first time this adapter has ever had a task a caller could see
	// before it finished, which is what makes cancelling meaningful at
	// all — see handleCancelTask.
	s.remember(taskID, taskRecord{Identity: identity, Task: task, cancel: cancel})

	go s.runAsync(runCtx, identity, p, skillID, def, run, task)

	// Enveloped like the blocking path: SendMessage's result is a
	// `task`|`message` oneof and the official client parses it as such.
	return sendMessageResult{Task: &task}, nil
}

func (s *Server) runAsync(
	ctx context.Context,
	identity learner.IdentityID,
	p sendMessageParams,
	skillID string,
	def skillDef,
	run agentrun.RunInput,
	task Task,
) {
	defer func() { <-s.asyncSlots }()

	out, runErr := s.runner.Run(ctx, run)
	s.forgetConsultations(out.RunID)

	// The finished task replaces the working one under the SAME id, so a
	// client that has been polling sees its state change rather than
	// having to discover a new task.
	//
	// settle, not remember: a cancelled run returns here with a
	// context-cancelled error, and writing that would overwrite the
	// CANCELED the user asked for with a FAILED nobody caused.
	s.settle(task.ID, applyOutcome(task, skillID, def, p.Configuration, out, runErr))
}
