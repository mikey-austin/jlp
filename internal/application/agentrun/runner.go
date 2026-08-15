// Package agentrun implements the agent-run loop (PRD §27/§50, Phase 4
// Task 2): the ONE place that drives an ai.ToolCaller conversation
// against internal/tools.Registry to convergence, owning every bit of
// tracing along the way. Agents (internal/agent/*) stay stateless and
// never see a Registry or an AgentRunRepository directly (Rule 3) —
// Runner is what an application service (e.g.
// application/feedback.Service's agentic teacher path) calls INSTEAD
// of driving a tool loop itself.
package agentrun

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// defaultMaxTurns is RunInput.MaxTurns' default when left at zero —
// the task brief's "hard cap, default 8": enough turns for a real
// investigate-then-answer conversation, low enough that a model stuck
// looping tool calls fails fast and visibly rather than burning cost
// silently.
const defaultMaxTurns = 8

// RunInput is everything Run needs to drive one agent-run loop.
// Messages is the conversation so far (typically just the rendered
// prompt's user turn); MaxTurns caps how many
// ai.ToolCaller.CallWithTools calls this run may make — zero means
// defaultMaxTurns, never zero calls.
type RunInput struct {
	Agent, PromptName, PromptVersion, System string
	Messages                                 []ai.ToolMessage
	Identity                                 learner.IdentityID
	SessionID                                *session.ID
	MaxTurns                                 int
}

// RunOutput is what Run returns. RunID identifies the agent_runs row
// and is set even when Run returns an error (a failed run still has a
// trace worth looking up — see /ai/agents/{id}); Text is the model's
// final turn (only meaningful on success); Turns is how many
// CallWithTools calls the run actually made.
type RunOutput struct {
	RunID string
	Text  string
	Turns int
}

// Runner drives one ai.ToolCaller conversation to convergence,
// recording an agent_runs row for the run and one tool_calls row per
// Registry.Invoke call along the way — EVERY Invoke result, including
// a refusal (unknown/disallowed tool) or a handler error, because "an
// agent tried to use a tool it isn't permitted" is exactly the audit
// event PRD §64 wants (see this package's tests pinning that a
// refused tool still produces a trace row and the run still
// completes).
type Runner struct {
	caller ai.ToolCaller
	reg    *tools.Registry
	runs   storage.AgentRunRepository
	clock  func() time.Time
}

// NewRunner wires a Runner. clock is injected (rather than time.Now
// called directly) so tests can control StartedAt/EndedAt/CreatedAt
// without depending on wall-clock timing.
func NewRunner(caller ai.ToolCaller, reg *tools.Registry, runs storage.AgentRunRepository, clock func() time.Time) *Runner {
	return &Runner{caller: caller, reg: reg, runs: runs, clock: clock}
}

// Run drives in's conversation: CallWithTools, then — for every
// invocation the model made — Registry.Invoke it (recording a
// tool_calls row per invocation regardless of outcome) and feed the
// results back, repeating until the model returns a turn with no
// invocations (success) or MaxTurns is exhausted while the model
// still wants to keep going (failure — never a silent truncation that
// looks like a normal answer, per the task brief).
//
// A failure to persist the trace itself — Start, or RecordToolCall for
// any single invocation — aborts the run immediately as "failed":
// PRD §64's audit trail is the entire point of this loop, so silently
// continuing a conversation whose trace has a hole in it is worse than
// failing loudly. Finish, by contrast, is best-effort (logged, not
// propagated): by the time it's called the run's real outcome is
// already decided, and failing to write the closing row shouldn't
// overwrite that outcome with an unrelated persistence error.
func (r *Runner) Run(ctx context.Context, in RunInput) (RunOutput, error) {
	maxTurns := in.MaxTurns
	if maxTurns <= 0 {
		maxTurns = defaultMaxTurns
	}

	runID := uuid.NewString()
	if err := r.runs.Start(ctx, storage.AgentRun{
		ID:            runID,
		IdentityID:    in.Identity,
		SessionID:     in.SessionID,
		Agent:         in.Agent,
		PromptName:    in.PromptName,
		PromptVersion: in.PromptVersion,
		Status:        "running",
		StartedAt:     r.clock(),
		System:        in.System,
		Input:         firstUserText(in.Messages),
	}); err != nil {
		return RunOutput{}, fmt.Errorf("agentrun: start: %w", err)
	}

	messages := append([]ai.ToolMessage(nil), in.Messages...)
	defs := r.reg.DefsFor(in.Agent)

	for turn := 1; turn <= maxTurns; turn++ {
		resp, err := r.caller.CallWithTools(ctx, ai.ToolRequest{
			PromptName:    in.PromptName,
			PromptVersion: in.PromptVersion,
			System:        in.System,
			Messages:      messages,
			Tools:         defs,
			IdentityID:    in.Identity,
			SessionID:     in.SessionID,
			Agent:         in.Agent,
		})
		if err != nil {
			callErr := fmt.Errorf("agentrun: call with tools: %w", err)
			r.finish(ctx, in.Identity, runID, "failed", callErr.Error(), "", turn)
			return RunOutput{RunID: runID}, callErr
		}

		messages = append(messages, ai.ToolMessage{
			Role:        "assistant",
			Text:        resp.Turn.Text,
			Invocations: resp.Turn.Invocations,
		})

		if len(resp.Turn.Invocations) == 0 {
			r.finish(ctx, in.Identity, runID, "completed", "", resp.Turn.Text, turn)
			return RunOutput{RunID: runID, Text: resp.Turn.Text, Turns: turn}, nil
		}

		results := make([]ai.ToolResult, 0, len(resp.Turn.Invocations))
		for _, inv := range resp.Turn.Invocations {
			callStart := r.clock()
			result := r.reg.Invoke(ctx, in.Agent, in.Identity, in.SessionID, inv)
			duration := r.clock().Sub(callStart)

			if err := r.runs.RecordToolCall(ctx, in.Identity, storage.ToolCall{
				ID:         uuid.NewString(),
				AgentRunID: runID,
				ToolName:   inv.Name,
				Arguments:  string(inv.Arguments),
				Result:     result.Content,
				IsError:    result.IsError,
				DurationMS: int(duration.Milliseconds()),
				CreatedAt:  r.clock(),
			}); err != nil {
				recordErr := fmt.Errorf("agentrun: record tool call %q: %w", inv.Name, err)
				r.finish(ctx, in.Identity, runID, "failed", recordErr.Error(), "", turn)
				return RunOutput{RunID: runID}, recordErr
			}

			results = append(results, result)
		}
		messages = append(messages, ai.ToolMessage{Role: "tool", Results: results})

		if turn == maxTurns {
			errMsg := fmt.Sprintf("agent run exceeded max turns (%d) while the model still requested tool calls", maxTurns)
			r.finish(ctx, in.Identity, runID, "failed", errMsg, "", turn)
			return RunOutput{RunID: runID}, fmt.Errorf("agentrun: %s", errMsg)
		}
	}

	// Unreachable: the loop above always returns on or before
	// turn == maxTurns.
	return RunOutput{RunID: runID}, fmt.Errorf("agentrun: exhausted max turns (%d) unexpectedly", maxTurns)
}

// finish persists runID's outcome. Failures are logged, not returned
// — see the Run doc comment's Start/RecordToolCall-vs-Finish
// distinction above.
func (r *Runner) finish(ctx context.Context, identity learner.IdentityID, runID, status, errMsg, output string, turns int) {
	if err := r.runs.Finish(ctx, identity, runID, status, errMsg, output, turns, r.clock()); err != nil {
		slog.Error("agentrun: finish failed", "run_id", runID, "status", status, "err", err)
	}
}

// firstUserText returns the first "user"-role message's Text in
// messages — the run's opening turn, stored as AgentRun.Input for the
// trace viewer's "input context" section. Every real caller
// (teacher.ReviewWritingAgentic) sends exactly one user message to
// start the conversation, but this tolerates a caller that sends more
// (or a leading non-user message) without panicking; returns "" if no
// user message is present at all.
func firstUserText(messages []ai.ToolMessage) string {
	for _, m := range messages {
		if m.Role == "user" {
			return m.Text
		}
	}
	return ""
}
