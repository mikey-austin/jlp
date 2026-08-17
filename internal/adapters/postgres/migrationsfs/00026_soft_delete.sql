-- +goose Up
-- Soft delete for the three things a learner accumulates and would want
-- to tidy: sessions, vocabulary_items and lessons (Phase 4 Task D).
--
-- A delete HIDES content; it never erases history. learning_events is
-- immutable and append-only — `jlp rebuild-model` replays the whole
-- stream — so removing events would retroactively rewrite the learner's
-- own statistics. Marking a row instead keeps /learner and /outcomes
-- exactly where they were while the row disappears from every list,
-- page, JSON API, agent tool and export. It is also what makes a delete
-- recoverable (`jlp restore`, and the undo affordance the list offers
-- straight after a delete).
--
-- NULL means live. There is deliberately no DEFAULT and no NOT NULL:
-- "deleted_at IS NULL" is the one predicate every read applies, and
-- db/queries' TestEveryQueryFiltersSoftDeleted (internal/adapters/
-- postgres/softdelete_guard_test.go) fails the build if a new query
-- against one of these tables forgets it.
ALTER TABLE sessions         ADD COLUMN deleted_at timestamptz;
ALTER TABLE vocabulary_items ADD COLUMN deleted_at timestamptz;
ALTER TABLE lessons          ADD COLUMN deleted_at timestamptz;

-- The list queries are all "this identity's LIVE rows, newest activity
-- first", so the filter belongs in the index predicate rather than as a
-- fourth key column: a partial index keeps the scan on the live rows
-- only and never grows with deleted ones. Same partial-index shape the
-- 00011 vocabulary_events_identity_client_event_idx already uses.
DROP INDEX sessions_identity_idx;
CREATE INDEX sessions_identity_idx
    ON sessions (identity_id, updated_at DESC)
    WHERE deleted_at IS NULL;

-- ...but sessions has one reader that deliberately does NOT filter, and
-- making the index above partial would have left it with nothing to
-- use: CountSessions (db/queries/analytics.sql) is the home dashboard's
-- cumulative セッション count and the weekly summary's "across N
-- sessions", and it counts EVERY session because the deleted ones still
-- happened. Without this second, unfiltered index it falls back to a
-- sequential scan on every home page load. Trivial at one learner's
-- scale, but it is a regression this migration would otherwise
-- introduce, so it does not.
--
-- lessons deliberately gets no equivalent: nothing reads it unfiltered
-- today, and an index with no reader is cost without benefit.
CREATE INDEX sessions_identity_all_idx ON sessions (identity_id);

DROP INDEX lessons_identity_idx;
CREATE INDEX lessons_identity_idx
    ON lessons (identity_id, created_at DESC)
    WHERE deleted_at IS NULL;

-- vocabulary_items had no index for its own list ordering at all — the
-- UNIQUE (identity_id, expression) constraint from 00011 serves the
-- by-expression lookups but not ListVocabularyItems' "ORDER BY
-- last_event DESC". Adding the filter is the moment to add it.
CREATE INDEX vocabulary_items_identity_last_event_idx
    ON vocabulary_items (identity_id, last_event DESC)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX sessions_identity_all_idx;
DROP INDEX vocabulary_items_identity_last_event_idx;

DROP INDEX lessons_identity_idx;
CREATE INDEX lessons_identity_idx ON lessons (identity_id, created_at DESC);

DROP INDEX sessions_identity_idx;
CREATE INDEX sessions_identity_idx ON sessions (identity_id, updated_at DESC);

ALTER TABLE lessons          DROP COLUMN deleted_at;
ALTER TABLE vocabulary_items DROP COLUMN deleted_at;
ALTER TABLE sessions         DROP COLUMN deleted_at;
