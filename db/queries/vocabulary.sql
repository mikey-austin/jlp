-- name: GetVocabularyEventItemByClientID :one
SELECT item_id FROM vocabulary_events
WHERE identity_id = $1 AND client_event_id = $2;

-- name: GetVocabularyItem :one
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags
FROM vocabulary_items
WHERE id = $1;

-- name: UpsertVocabularyItemOnLookup :one
INSERT INTO vocabulary_items
    (id, identity_id, expression, reading, meaning, kind, source, lookups, productions, successful_productions, first_seen, last_event)
VALUES ($1, $2, $3, $4, $5, $6, $7, 1, 0, 0, $8, $8)
ON CONFLICT (identity_id, expression) DO UPDATE SET
    lookups    = vocabulary_items.lookups + 1,
    last_event = EXCLUDED.last_event,
    reading    = CASE WHEN EXCLUDED.reading <> '' THEN EXCLUDED.reading ELSE vocabulary_items.reading END,
    meaning    = CASE WHEN EXCLUDED.meaning <> '' THEN EXCLUDED.meaning ELSE vocabulary_items.meaning END,
    source     = CASE WHEN EXCLUDED.source  <> '' THEN EXCLUDED.source  ELSE vocabulary_items.source  END
RETURNING id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
          lookups, productions, successful_productions, first_seen, last_event,
          meaning_en, tags;

-- name: InsertVocabularyEvent :exec
INSERT INTO vocabulary_events (id, identity_id, item_id, type, payload, client_event_id, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: InsertVocabularyItemIfAbsent :exec
-- Task 7's expression-bank seed path: unlike UpsertVocabularyItemOnLookup,
-- a conflict on (identity_id, expression) is a silent no-op — an
-- existing item's lookups/productions/reading/etc. are never touched.
-- This is what makes `jlp seed` re-runnable without resetting a
-- learner's real usage of a bank expression back to zero.
INSERT INTO vocabulary_items
    (id, identity_id, expression, reading, meaning, kind, source, lookups, productions, successful_productions, first_seen, last_event)
VALUES ($1, $2, $3, $4, $5, $6, $7, 0, 0, 0, $8, $8)
ON CONFLICT (identity_id, expression) DO NOTHING;

-- name: RecordVocabularyProduction :exec
UPDATE vocabulary_items SET
    productions            = productions + 1,
    successful_productions = successful_productions + CASE WHEN sqlc.arg(successful)::boolean THEN 1 ELSE 0 END,
    last_event              = sqlc.arg(at)::timestamptz
WHERE id = sqlc.arg(id) AND identity_id = sqlc.arg(identity_id);

-- name: ListVocabularyItems :many
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags
FROM vocabulary_items
WHERE identity_id = $1
  AND (
        sqlc.arg(filter)::text = ''
        OR (sqlc.arg(filter)::text = 'looked-up' AND lookups > 0)
        OR (sqlc.arg(filter)::text = 'produced' AND productions > 0)
        OR (sqlc.arg(filter)::text = 'activate' AND (
              (lookups >= 3 AND productions = 0)
              OR (kind IN ('expression', 'pattern') AND productions = 0)
            ))
      )
ORDER BY last_event DESC;

-- name: ListVocabularyActivationCandidates :many
-- PRD §55/§17.5's vocabulary activator, ranked and capped in SQL — the
-- same WHERE condition ListVocabularyItems' "activate" branch uses
-- above, but with ORDER BY lookups DESC LIMIT pushed down here instead
-- of fetched-then-sorted in Go, matching the ORDER BY ... LIMIT $n
-- convention TopPriorities already uses for the analogous "top N"
-- query over learner_priorities. application/planner.Planner.
-- ActivationCandidates is a thin passthrough to this (not to
-- ListVocabularyItems), precisely so it stays cheap to call on every
-- feedback request even as a learner's vocabulary grows.
-- limit_count = 0 means unlimited (NULLIF makes LIMIT NULL, i.e. no
-- cap) — see application/planner.Planner.ActivationCandidates' doc
-- comment for why limit<=0 is a documented "no cap" affordance.
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags
FROM vocabulary_items
WHERE identity_id = $1
  AND (
        (lookups >= 3 AND productions = 0)
        OR (kind IN ('expression', 'pattern') AND productions = 0)
      )
ORDER BY lookups DESC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);

-- name: ListVocabularyExpressions :many
SELECT id, expression FROM vocabulary_items WHERE identity_id = $1;

-- name: UpsertVocabularyWord :exec
-- Phase 3 Task 8's bulk sync path (POST /api/v1/words): unlike
-- UpsertVocabularyItemOnLookup above, lookups/productions/
-- successful_productions are NEVER touched by this — a sync is not a
-- lookup event, so they stay at their existing value (0 on first
-- insert). Every string field does a sparse merge (empty incoming
-- value keeps the existing one; jlpt_level 0 keeps the existing
-- level) EXCEPT tags, which replaces wholesale when the incoming
-- array is non-empty rather than merging entry-by-entry.
INSERT INTO vocabulary_items
    (id, identity_id, expression, reading, meaning, meaning_en, kind, jlpt_level, source, tags, lookups, productions, successful_productions, first_seen, last_event)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 0, 0, 0, $11, $11)
ON CONFLICT (identity_id, expression) DO UPDATE SET
    reading    = CASE WHEN EXCLUDED.reading    <> '' THEN EXCLUDED.reading    ELSE vocabulary_items.reading    END,
    meaning    = CASE WHEN EXCLUDED.meaning    <> '' THEN EXCLUDED.meaning    ELSE vocabulary_items.meaning    END,
    meaning_en = CASE WHEN EXCLUDED.meaning_en <> '' THEN EXCLUDED.meaning_en ELSE vocabulary_items.meaning_en END,
    jlpt_level = CASE WHEN EXCLUDED.jlpt_level <> 0  THEN EXCLUDED.jlpt_level ELSE vocabulary_items.jlpt_level END,
    source     = CASE WHEN EXCLUDED.source     <> '' THEN EXCLUDED.source     ELSE vocabulary_items.source     END,
    tags       = CASE WHEN jsonb_array_length(EXCLUDED.tags) > 0 THEN EXCLUDED.tags ELSE vocabulary_items.tags END,
    last_event = EXCLUDED.last_event;
