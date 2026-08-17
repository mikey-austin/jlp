-- Soft delete (Phase 4 Task D): anki_cards deliberately does NOT
-- cascade, and this is a decision rather than an omission.
--
-- front/back/notes are DENORMALISED copies of a correction's original,
-- replacement and explanation, written once by application/anki's
-- GenerateFromCorrection and never re-read from corrections. So a card
-- made from a session the learner later deletes keeps showing that
-- session's sentence on /anki and in the TSV export. Filtering here
-- would fix that — and would also silently destroy flashcards the
-- learner explicitly reviewed and approved, as a side effect of tidying
-- a session. Deleting a session is not a request to delete your deck.
--
-- What DID change: GetCorrection now filters, so a card can no longer
-- be CREATED from a deleted session's correction. New cards stop; old
-- cards stay. If a learner should be able to remove cards, that is its
-- own delete affordance on /anki, with its own confirmation — not a
-- cascade they never asked for.
--
-- anki_cards is absent from softdelete_guard_test.go's guardedTables
-- for the same reason; this comment is the record of why.

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
