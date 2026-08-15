-- +goose Up
CREATE TABLE agent_runs (
    id             uuid PRIMARY KEY,
    identity_id    text NOT NULL REFERENCES identities(id),
    session_id     uuid REFERENCES sessions(id),
    agent          text NOT NULL,
    prompt_name    text NOT NULL,
    prompt_version text NOT NULL DEFAULT '',
    status         text NOT NULL DEFAULT 'running',
    turns          int NOT NULL DEFAULT 0,
    started_at     timestamptz NOT NULL,
    ended_at       timestamptz,
    error          text NOT NULL DEFAULT ''
);
CREATE INDEX agent_runs_identity_started_idx ON agent_runs (identity_id, started_at DESC);

-- tool_calls has no identity_id of its own — every access is scoped
-- via a join back to its owning agent_runs row (see
-- ports/storage/agentruns.go's AgentRunRepository doc comment), the
-- same identity-scoped-join pattern feedback_requests/corrections
-- uses (see the 00006 migration).
CREATE TABLE tool_calls (
    id           uuid PRIMARY KEY,
    agent_run_id uuid NOT NULL REFERENCES agent_runs(id),
    tool_name    text NOT NULL,
    arguments    text NOT NULL DEFAULT '',
    result       text NOT NULL DEFAULT '',
    is_error     boolean NOT NULL DEFAULT false,
    duration_ms  int NOT NULL DEFAULT 0,
    created_at   timestamptz NOT NULL
);
CREATE INDEX tool_calls_agent_run_idx ON tool_calls (agent_run_id, created_at ASC);

-- +goose Down
DROP TABLE tool_calls;
DROP TABLE agent_runs;
