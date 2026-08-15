-- name: InsertAgentRun :exec
INSERT INTO agent_runs (
    id, identity_id, session_id, agent, prompt_name, prompt_version,
    status, turns, started_at, system, input
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

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
SELECT id, identity_id, session_id, agent, prompt_name, prompt_version,
       status, turns, started_at, ended_at, error, system, input, output
FROM agent_runs
WHERE identity_id = $1
ORDER BY started_at DESC
LIMIT $2;

-- name: GetAgentRun :one
SELECT id, identity_id, session_id, agent, prompt_name, prompt_version,
       status, turns, started_at, ended_at, error, system, input, output
FROM agent_runs
WHERE id = $1 AND identity_id = $2;

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
