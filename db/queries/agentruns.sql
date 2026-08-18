-- name: InsertAgentRun :exec
INSERT INTO agent_runs (
    id, identity_id, session_id, agent, prompt_name, prompt_version,
    status, turns, started_at, system, input, parent_run_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, sqlc.narg(parent_run_id));

-- name: FinishAgentRun :execrows
UPDATE agent_runs
SET status = $3, error = $4, output = $5, turns = $6, ended_at = $7
WHERE id = $1 AND identity_id = $2;

-- name: InsertToolCall :execrows
-- Identity-scoped via a join back to agent_runs rather than a plain
-- INSERT: a c.AgentRunID that doesn't exist, or belongs to a different
-- identity, must insert nothing, and RowsAffected()==0 is how the
-- adapter tells that apart from success.
INSERT INTO tool_calls (id, agent_run_id, tool_name, arguments, result, is_error, duration_ms, created_at)
SELECT $1, ar.id, $2, $3, $4, $5, $6, $7
FROM agent_runs ar
WHERE ar.id = $8 AND ar.identity_id = $9;

-- name: ListAgentRuns :many
-- Soft delete, session cascade (Phase 4 Task D) — see the header
-- comment in db/queries/documents.sql for the shape.
--
-- agent_runs looks like an operational record, and mostly is, but
-- system/input/output hold the learner's own text VERBATIM: for a
-- feedback run, input is the selection they asked to have reviewed
-- (application/agentrun.Runner sets it from the first user message),
-- and a get_recent_writing tool call's result carries up to 1000 runes
-- of that session's document. /ai/agents renders all of it. Filtering
-- an operational record felt unnecessary until you notice it is a
-- second, unindexed copy of the writing.
--
-- session_id is nullable (00018), and a NULL one belongs to no session
-- — lesson generation, the weekly summary — so it must stay visible;
-- that is what the "IS NULL OR" half says. No statistic reads
-- agent_runs: AgentUsageStats is over ai_requests, so nothing on
-- /learner moves.
SELECT id, identity_id, session_id, agent, prompt_name, prompt_version,
       status, turns, started_at, ended_at, error, system, input, output,
       parent_run_id
FROM agent_runs
WHERE agent_runs.identity_id = $1
  AND (agent_runs.session_id IS NULL
       OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = agent_runs.session_id AND s.deleted_at IS NULL))
ORDER BY started_at DESC
LIMIT $2;

-- name: GetAgentRun :one
-- Same filter as ListAgentRuns above, and the reason the two child
-- queries below need none of their own: /ai/agents/{id} resolves the
-- run through this query first, so a deleted session's trace 404s
-- before its tool calls or turns are ever fetched.
SELECT id, identity_id, session_id, agent, prompt_name, prompt_version,
       status, turns, started_at, ended_at, error, system, input, output,
       parent_run_id
FROM agent_runs
WHERE agent_runs.id = $1 AND agent_runs.identity_id = $2
  AND (agent_runs.session_id IS NULL
       OR EXISTS (SELECT 1 FROM sessions s WHERE s.id = agent_runs.session_id AND s.deleted_at IS NULL));

-- name: ListToolCallsForRun :many
SELECT id, agent_run_id, tool_name, arguments, result, is_error, duration_ms, created_at
FROM tool_calls
WHERE agent_run_id = $1
ORDER BY created_at ASC;

-- name: InsertAgentTurn :execrows
-- Identity-scoped via a join back to agent_runs, same convention as
-- InsertToolCall above.
INSERT INTO agent_turns (id, agent_run_id, turn_number, text, created_at)
SELECT $1, ar.id, $2, $3, $4
FROM agent_runs ar
WHERE ar.id = $5 AND ar.identity_id = $6;

-- name: ListAgentTurnsForRun :many
SELECT id, agent_run_id, turn_number, text, created_at
FROM agent_turns
WHERE agent_run_id = $1
ORDER BY turn_number ASC;
