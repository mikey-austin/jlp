-- +goose Up
-- The /ai/agents trace viewer (Phase 4 Task 2, PRD §27/§50) needs to
-- show the conversation's input context and final answer alongside the
-- tool_calls the 00018 migration already captures — neither was
-- persisted by Task 1's schema. system/input are written once, at
-- Start; output is written once, at Finish, and stays '' for a failed
-- run (its error column already explains what happened instead).
ALTER TABLE agent_runs ADD COLUMN system text NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN input text NOT NULL DEFAULT '';
ALTER TABLE agent_runs ADD COLUMN output text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE agent_runs DROP COLUMN output;
ALTER TABLE agent_runs DROP COLUMN input;
ALTER TABLE agent_runs DROP COLUMN system;
