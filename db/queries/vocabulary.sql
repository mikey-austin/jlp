-- name: GetVocabularyEventItemByClientID :one
SELECT item_id FROM vocabulary_events
WHERE identity_id = $1 AND client_event_id = $2;

-- name: GetVocabularyItem :one
-- DELIBERATELY sees soft-deleted rows, and is the only vocabulary read
-- that does. Its single caller is the client_event_id replay branch
-- inside postgres/vocabulary.go's UpsertOnLookup: the item id comes
-- from the vocabulary_events row this exact client event already wrote,
-- and the contract is "a retried POST returns what the first one
-- returned, unchanged". Filtering here would turn a replayed lookup of
-- a since-deleted word into an error instead of the same idempotent
-- response. Nothing renders this row to a learner — Ingest's own return
-- value goes back to the API caller that just retried.
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
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
    source     = CASE WHEN EXCLUDED.source  <> '' THEN EXCLUDED.source  ELSE vocabulary_items.source  END,
    -- Soft delete (Phase 4 Task D): a fresh lookup RESURRECTS a
    -- deleted word. UNIQUE (identity_id, expression) means the
    -- conflict lands on the deleted row, so the only alternative is
    -- leaving deleted_at set — and then a learner who looks the word
    -- up again in the browser extension sees their lookup vanish into
    -- a row they cannot reach, with no way to tell why. A word coming
    -- back after you deliberately looked it up again is visible and
    -- undoable; a lookup silently going nowhere is neither.
    --
    -- This applies ONLY to this query — one expression at a time.
    -- UpsertVocabularyWord below (the bulk deck sync) and
    -- InsertVocabularyItemIfAbsent (the expression-bank seed) both
    -- leave deleted_at alone, because a delete must survive the next
    -- automatic sync — otherwise deleting a synced word would be
    -- permanently impossible.
    --
    -- Be precise about who "the learner" is here: resurrect is a
    -- property of THIS QUERY, not of any particular caller, and it has
    -- two — POST /api/v1/vocabulary/events and the MQTT bridge, whose
    -- identity comes from the topic (learner/{id}/vocabulary/ingest)
    -- with no per-message auth. So a broker publisher, including a
    -- retained message replayed on reconnect, can undo a word delete.
    -- That is a consequence of the MQTT channel's existing trust model,
    -- not something resurrect introduces: the same publisher can
    -- already CREATE vocabulary for that identity, which is strictly
    -- more than bringing one back. Worth knowing before that channel is
    -- exposed beyond a trusted LAN broker.
    deleted_at = NULL
RETURNING id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
          lookups, productions, successful_productions, first_seen, last_event,
          meaning_en, tags, deleted_at;

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
WHERE id = sqlc.arg(id) AND identity_id = sqlc.arg(identity_id) AND deleted_at IS NULL;

-- ListVocabularyItemsPage is /vocabulary's list: one keyset page,
-- newest activity first. See migration 00028 for why this is a cursor
-- rather than an OFFSET, and why the (last_event, id) tiebreaker is
-- load-bearing rather than tidy.
--
-- The cursor arguments are passed together or not at all: NULL
-- last_event means "from the beginning". The row comparison
-- (last_event, id) < (cursor_last_event, cursor_id) is a single tuple
-- comparison so it can use the matching index directly, rather than the
-- OR-chain spelling of the same predicate, which cannot.
-- name: ListVocabularyItemsPage :many
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1
  AND deleted_at IS NULL
  AND (
        sqlc.arg(filter)::text = ''
        OR (sqlc.arg(filter)::text = 'looked-up' AND lookups > 0)
        OR (sqlc.arg(filter)::text = 'produced' AND productions > 0)
        OR (sqlc.arg(filter)::text = 'activate' AND (
              (lookups >= 3 AND productions = 0)
              OR (kind IN ('expression', 'pattern') AND productions = 0)
            ))
      )
  AND (
        sqlc.narg(cursor_last_event)::timestamptz IS NULL
        OR (last_event, id) < (sqlc.narg(cursor_last_event)::timestamptz, sqlc.narg(cursor_id)::uuid)
      )
ORDER BY last_event DESC, id DESC
LIMIT sqlc.arg(page_size);

-- ListVocabularyItems returns EVERY matching item, unpaged. Kept for
-- callers that genuinely need the whole set in one go (the Anki export
-- and the agent tool registry); page the UI with
-- ListVocabularyItemsPage above instead.
-- name: ListVocabularyItems :many
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1
  AND deleted_at IS NULL
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
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1
  AND deleted_at IS NULL
  AND (
        (lookups >= 3 AND productions = 0)
        OR (kind IN ('expression', 'pattern') AND productions = 0)
      )
ORDER BY lookups DESC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);

-- name: ListVocabularyExpressions :many
-- The candidate set application/vocabulary.Service.DetectProduction
-- scans reviewed text against. The deleted_at filter is not cosmetic
-- here: a deleted word left in this set would keep collecting
-- vocabulary.produced / vocabulary.produced-correctly events and keep
-- re-scheduling itself in retrieval_items, so a word the learner
-- removed would go on generating work for them from a row they cannot
-- see.
SELECT id, expression FROM vocabulary_items WHERE identity_id = $1 AND deleted_at IS NULL;

-- name: GetVocabularyItemsByExpressions :many
-- Bounded lookup for a SMALL, caller-supplied set of expressions —
-- application/feedback.Service.dueExpressionItems' resolve-a-due-
-- subject-back-to-Reading/Meaning step (PRD §54): unlike
-- ListVocabularyItems (filter=""), which scans the identity's ENTIRE
-- vocabulary, this is indexed on (identity_id, expression) and returns
-- at most len(expressions) rows — the same "push the bound into SQL"
-- principle ListVocabularyActivationCandidates documents above, so
-- resolving a handful of due expressions stays cheap regardless of how
-- large a learner's vocabulary grows.
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1 AND deleted_at IS NULL AND expression = ANY(sqlc.arg(expressions)::text[]);

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
-- deleted_at is deliberately absent from that SET list. A bulk deck
-- sync must never undo a delete: if it did, a word the learner removed
-- would return on the very next sync and could never be got rid of.
-- Only UpsertVocabularyItemOnLookup — one expression, looked up by the
-- learner on purpose, right now — clears deleted_at. See its comment.

-- name: SoftDeleteVocabularyItem :execrows
-- The learner's "delete this word" (Phase 4 Task D). Identity-scoped,
-- idempotent, and non-destructive for exactly the reasons
-- db/queries/sessions.sql's SoftDeleteSession spells out — read that
-- comment; this is the same statement over a different table, on
-- purpose, so there is one shape to understand rather than three.
--
-- The vocabulary_events rows behind the item are untouched, as are the
-- learning_events its lookups and productions produced: /learner's
-- vocabulary funnel and /outcomes are computed from those and must not
-- move when a learner tidies their word list.
UPDATE vocabulary_items SET deleted_at = COALESCE(deleted_at, sqlc.arg(at)::timestamptz)
WHERE id = $1 AND identity_id = $2;

-- name: RestoreVocabularyItem :execrows
-- The way back — see SoftDeleteSession/RestoreSession in
-- db/queries/sessions.sql.
UPDATE vocabulary_items SET deleted_at = NULL
WHERE id = $1 AND identity_id = $2;

-- name: ListRecentUnpracticedVocabulary :many
-- 練習's first choice of what to drill (see
-- application/practice.Service.Start): words added recently that the
-- learner has never produced correctly, newest first.
--
-- Recency leads the whole selection order because a word added this
-- week still has the context that produced it attached — the sentence
-- it came from, why it was looked up — and that is the moment it is
-- cheapest to learn. Everything past that window is the SRS
-- scheduler's job, and stays so.
--
-- Both bounds are load-bearing. Without first_seen >= $2 every drill
-- is a word forever; without successful_productions = 0 the same word
-- repeats until its SRS interval catches up.
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1
  AND deleted_at IS NULL
  AND first_seen >= sqlc.arg(added_since)
  AND successful_productions = 0
ORDER BY first_seen DESC
LIMIT sqlc.arg(limit_count);

-- name: GetVocabularyItemsByIDs :many
-- Resolves a small, caller-supplied set of vocabulary IDs — the bounded
-- indexed sibling of GetVocabularyItemsByExpressions above, for callers
-- that hold an ID rather than a surface form. 練習 is the one today: a
-- retrieval_items row due for review carries the vocabulary ID as its
-- subject, and the drill needs the word itself to build a card.
--
-- deleted_at IS NULL, unlike GetVocabularyItem's deliberate exception:
-- nothing here is replaying an idempotent write, and a word the learner
-- removed must not come back as a drill.
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event,
       meaning_en, tags, deleted_at
FROM vocabulary_items
WHERE identity_id = $1 AND deleted_at IS NULL AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: LatestVocabularyExamples :many
-- The most recent example sentence recorded for each of the given
-- vocabulary items, from the lookup events that carried them.
--
-- This is where example sentences live: UpsertOnLookup writes them into
-- vocabulary_events.payload, and vocabulary_items has no column for
-- one. That is the right place for 練習 to read them from — the sentence
-- the learner ACTUALLY met the word in, from their own reading, rather
-- than one invented for the drill.
--
-- DISTINCT ON with the ordering below picks the newest non-empty
-- example per item: a word looked up three times keeps the sentence
-- from the most recent encounter, which is the one still fresh.
SELECT DISTINCT ON (e.item_id)
       e.item_id,
       e.payload->>'example' AS example
FROM vocabulary_events e
JOIN vocabulary_items i ON i.id = e.item_id
WHERE e.identity_id = $1
  AND i.deleted_at IS NULL
  AND e.item_id = ANY(sqlc.arg(ids)::uuid[])
  AND COALESCE(e.payload->>'example', '') <> ''
ORDER BY e.item_id, e.occurred_at DESC;
