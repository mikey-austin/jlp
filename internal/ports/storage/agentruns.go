package storage

import (
	"context"
	"time"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
)

// AgentRun is one trace of an agent-run loop (PRD §27/§50): one call
// into internal/tools.Registry-mediated, multi-turn ToolCaller
// conversation, from Start through Finish. Status starts "running" and
// Finish moves it to "completed" or "failed" exactly once — Turns and
// EndedAt are both zero/unset until then.
type AgentRun struct {
	ID                               string
	IdentityID                       learner.IdentityID
	SessionID                        *session.ID
	Agent, PromptName, PromptVersion string
	Status                           string // "running" | "completed" | "failed"
	Turns                            int
	StartedAt, EndedAt               time.Time
	Error                            string
	// System/Input are the conversation's starting point, written once
	// at Start (System is the rendered prompt's system text; Input is
	// its opening user turn) — the /ai/agents trace viewer's "input
	// context" section. Output is the model's final turn text, written
	// once at Finish and only ever non-empty for a "completed" run: a
	// "failed" run's Error already explains what happened instead.
	System, Input, Output string
}

// ToolCall is one internal/tools.Registry.Invoke call made during an
// AgentRun: Arguments/Result are the exact JSON strings that crossed
// the model boundary (the model's raw arguments, and the tool's
// compact string output or error message) — kept as plain text
// columns, not jsonb, since they're already model-facing strings, not
// structured data this package ever queries into.
type ToolCall struct {
	ID, AgentRunID, ToolName string
	Arguments, Result        string
	IsError                  bool
	DurationMS               int
	CreatedAt                time.Time
}

// AgentRunRepository persists agent-run traces: agent_runs (one row
// per run) and tool_calls (one row per Registry.Invoke call within a
// run) — see the 00018 migration. tool_calls carries no identity_id of
// its own: every tool_calls access is scoped via a join back to its
// owning agent_runs row, the same identity-scoped-join convention
// storage.FeedbackRepository's corrections use via feedback_requests
// (see that package's doc comment).
type AgentRunRepository interface {
	// Start persists a newly begun run, Status "running".
	Start(ctx context.Context, run AgentRun) error
	// Finish sets runID's Status/Error/Output/Turns/EndedAt —
	// identity-scoped: a runID that exists but belongs to a different
	// identity misses with ErrNotFound, matching every other
	// identity-scoped write in this package. output is the model's
	// final turn text (empty for a failed run — see AgentRun.Output's
	// doc comment).
	Finish(ctx context.Context, identity learner.IdentityID, runID, status, errMsg, output string, turns int, endedAt time.Time) error
	// RecordToolCall persists c, scoped via a join to agent_runs: a
	// c.AgentRunID that exists but belongs to a different identity — or
	// doesn't exist at all — misses with ErrNotFound and writes nothing,
	// same as Finish.
	RecordToolCall(ctx context.Context, identity learner.IdentityID, c ToolCall) error
	// List returns up to limit of identity's runs, newest (by
	// StartedAt) first.
	List(ctx context.Context, identity learner.IdentityID, limit int) ([]AgentRun, error)
	// Get reads back one run and every tool call recorded against it,
	// oldest first (replay order). A runID that exists but belongs to a
	// different identity misses with ErrNotFound, same as Finish.
	Get(ctx context.Context, identity learner.IdentityID, runID string) (AgentRun, []ToolCall, error)
}
