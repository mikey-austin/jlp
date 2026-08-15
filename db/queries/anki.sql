-- name: InsertAnkiCard :exec
INSERT INTO anki_cards (id, identity_id, source_type, source_id, front, back, notes, status, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListAnkiCards :many
-- status "" (sqlc.arg(status) = '') means "every status" — mirrors
-- ListVocabularyItems' own empty-string-means-unfiltered convention.
SELECT id, identity_id, source_type, source_id, front, back, notes, status, created_at
FROM anki_cards
WHERE identity_id = $1
  AND (sqlc.arg(status)::text = '' OR status = sqlc.arg(status))
ORDER BY created_at DESC;

-- name: UpdateAnkiCardStatus :one
UPDATE anki_cards SET status = $3
WHERE id = $1 AND identity_id = $2
RETURNING id, identity_id, source_type, source_id, front, back, notes, status, created_at;

-- name: SelectApprovedAnkiCardsForUpdate :many
-- Row-locking read half of TakeApprovedForExport (see
-- storage.AnkiCardRepository's doc comment): postgres/anki.go runs this
-- and MarkAnkiCardsExportedByIDs below in ONE transaction. FOR UPDATE
-- takes an exclusive row lock on every matched row, so a second,
-- concurrent call for the SAME identity blocks here until the first
-- transaction commits (or rolls back) — at which point the rows it
-- locked either no longer match status = 'approved' (first call
-- succeeded: second call sees nothing) or are visible again unchanged
-- (first call rolled back).
SELECT id, identity_id, source_type, source_id, front, back, notes, status, created_at
FROM anki_cards
WHERE identity_id = $1 AND status = 'approved'
ORDER BY created_at ASC
FOR UPDATE;

-- name: MarkAnkiCardsExportedByIDs :exec
-- Write half of TakeApprovedForExport, run in the SAME transaction as
-- SelectApprovedAnkiCardsForUpdate above, against exactly the ids that
-- query just locked and returned. status = 'approved' is kept as a
-- belt-and-suspenders guard (defense in depth, not load-bearing given
-- the FOR UPDATE lock already serializes access).
UPDATE anki_cards SET status = 'exported'
WHERE identity_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]) AND status = 'approved';
