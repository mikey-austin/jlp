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

-- name: ApprovedAnkiCardsForExport :many
SELECT id, identity_id, source_type, source_id, front, back, notes, status, created_at
FROM anki_cards
WHERE identity_id = $1 AND status = 'approved'
ORDER BY created_at ASC;

-- name: MarkAnkiCardsExported :exec
-- Restricted to status = 'approved' so calling this twice with the same
-- ids (e.g. a retried request) is a safe no-op the second time.
UPDATE anki_cards SET status = 'exported'
WHERE identity_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]) AND status = 'approved';
