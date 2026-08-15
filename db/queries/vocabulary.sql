-- name: GetVocabularyEventItemByClientID :one
SELECT item_id FROM vocabulary_events
WHERE identity_id = $1 AND client_event_id = $2;

-- name: GetVocabularyItem :one
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event
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
          lookups, productions, successful_productions, first_seen, last_event;

-- name: InsertVocabularyEvent :exec
INSERT INTO vocabulary_events (id, identity_id, item_id, type, payload, client_event_id, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: RecordVocabularyProduction :exec
UPDATE vocabulary_items SET
    productions            = productions + 1,
    successful_productions = successful_productions + CASE WHEN sqlc.arg(successful)::boolean THEN 1 ELSE 0 END,
    last_event              = sqlc.arg(at)::timestamptz
WHERE id = sqlc.arg(id) AND identity_id = sqlc.arg(identity_id);

-- name: ListVocabularyItems :many
SELECT id, identity_id, expression, reading, meaning, kind, jlpt_level, source,
       lookups, productions, successful_productions, first_seen, last_event
FROM vocabulary_items
WHERE identity_id = $1
  AND (
        sqlc.arg(filter)::text = ''
        OR (sqlc.arg(filter)::text = 'looked-up' AND lookups > 0)
        OR (sqlc.arg(filter)::text = 'produced' AND productions > 0)
        OR (sqlc.arg(filter)::text = 'activate' AND false) -- Task 7 wires this up; empty until then
      )
ORDER BY last_event DESC;

-- name: ListVocabularyExpressions :many
SELECT id, expression FROM vocabulary_items WHERE identity_id = $1;
