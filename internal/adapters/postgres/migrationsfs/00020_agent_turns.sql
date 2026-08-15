-- +goose Up
-- Independent review of Phase 4 Task 2 found the trace viewer missing
-- the brief's "each model turn" requirement: only the FINAL turn's
-- text was persisted (agent_runs.output), so an intermediate turn's
-- prose — and every turn's text on a run that fails MaxTurns, since
-- output is only ever written on success — was silently discarded.
-- agent_turns is tool_calls' sibling: one row per
-- ai.ToolCaller.CallWithTools response the agent-run loop received,
-- ordered the same way (a join back to agent_runs, no identity_id of
-- its own — see ports/storage/agentruns.go's AgentRunRepository doc
-- comment for the shared reasoning).
CREATE TABLE agent_turns (
    id           uuid PRIMARY KEY,
    agent_run_id uuid NOT NULL REFERENCES agent_runs(id),
    turn_number  int NOT NULL,
    text         text NOT NULL DEFAULT '',
    created_at   timestamptz NOT NULL
);
CREATE INDEX agent_turns_agent_run_idx ON agent_turns (agent_run_id, turn_number ASC);

-- +goose Down
DROP TABLE agent_turns;
