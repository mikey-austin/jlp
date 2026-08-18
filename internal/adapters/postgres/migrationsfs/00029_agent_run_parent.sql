-- +goose Up
-- A delegated agent run has a cause: the run that consulted it.
--
-- Without this, /ai/agents shows a second run appearing from nowhere
-- alongside the one the learner asked for, and "why did this question
-- cost two agent runs" cannot be answered from the trace — which is the
-- question the trace viewer exists to answer.
--
-- Nullable because almost every run is top-level. Self-referencing, with
-- ON DELETE SET NULL rather than CASCADE: if a parent run is ever
-- removed, the child's own record of what it did and what it cost is
-- still worth keeping, it has simply lost its explanation.
ALTER TABLE agent_runs
    ADD COLUMN parent_run_id uuid REFERENCES agent_runs(id) ON DELETE SET NULL;

-- The trace viewer's question is "what did this run spawn", so the index
-- is on the parent. Partial: only delegated runs have one, and today
-- that is a small minority of rows.
CREATE INDEX agent_runs_parent_idx
    ON agent_runs (parent_run_id)
    WHERE parent_run_id IS NOT NULL;

-- +goose Down
DROP INDEX agent_runs_parent_idx;
ALTER TABLE agent_runs DROP COLUMN parent_run_id;
