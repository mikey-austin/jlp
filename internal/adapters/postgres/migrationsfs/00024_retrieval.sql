-- +goose Up
-- retrieval_items backs Phase 4 Task 7's spaced-retrieval scheduler
-- (PRD §54): one row per (identity, subject_type, subject) — a grammar
-- concept slug or a vocabulary expression — maintained entirely by
-- internal/application/retrieval.Scheduler.RecordOutcome. The UNIQUE
-- constraint is what Upsert's ON CONFLICT targets, the same shape the
-- 00009 learner_observations migration already established for an
-- analogous "one row per subject" table.
CREATE TABLE retrieval_items (
    identity_id  text NOT NULL REFERENCES identities(id),
    subject_type text NOT NULL,
    subject      text NOT NULL,
    successes    int NOT NULL DEFAULT 0,
    failures     int NOT NULL DEFAULT 0,
    last_seen    timestamptz NOT NULL,
    due_at       timestamptz NOT NULL,
    interval_seconds bigint NOT NULL,
    confidence   float8 NOT NULL DEFAULT 0,
    PRIMARY KEY (identity_id, subject_type, subject)
);
-- Backs Due's "identity's items due by <at>, earliest first" query and
-- List's "every item, earliest due first" query.
CREATE INDEX retrieval_items_identity_due_idx ON retrieval_items (identity_id, due_at);

-- +goose Down
DROP TABLE retrieval_items;
