-- Soft delete, vocabulary cascade (Phase 4 Task D).
--
-- retrieval_items has no foreign key to vocabulary_items: it is keyed
-- by (identity_id, subject_type, subject) TEXT, where an "expression"
-- subject IS the vocabulary expression. That makes it the one place a
-- deleted word can still surface by name — on /learner's 復習キュー,
-- and in the due queue that feeds feedback requests and practice — so
-- the two read queries below LEFT JOIN back to vocabulary_items and
-- drop the row when the matching word is deleted.
--
-- LEFT JOIN, not a plain join: a subject with NO vocabulary row at all
-- (every "concept" subject, and any expression scheduled without one)
-- must be untouched, which is what the "v.identity_id IS NULL OR" half
-- of the predicate says. UNIQUE (identity_id, expression) on
-- vocabulary_items means the join can never fan a retrieval item out
-- into two rows.
--
-- This is the one place where hiding content DOES change something on
-- /learner, and deliberately: the review queue is a forward-looking
-- work list, not a record of practice that happened. Continuing to ask
-- a learner to review a word they deleted — from a row they cannot see
-- — is the same invisible-work-generation problem the deleted_at filter
-- on ListVocabularyExpressions exists to prevent. The statistics on
-- /learner (the funnel, the event counts) are untouched.

-- name: UpsertRetrievalItem :exec
INSERT INTO retrieval_items (identity_id, subject_type, subject, successes, failures, last_seen, due_at, interval_seconds, confidence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (identity_id, subject_type, subject) DO UPDATE SET
    successes        = EXCLUDED.successes,
    failures         = EXCLUDED.failures,
    last_seen        = EXCLUDED.last_seen,
    due_at           = EXCLUDED.due_at,
    interval_seconds = EXCLUDED.interval_seconds,
    confidence       = EXCLUDED.confidence;

-- name: GetRetrievalItem :one
SELECT identity_id, subject_type, subject, successes, failures, last_seen, due_at, interval_seconds, confidence
FROM retrieval_items
WHERE identity_id = $1 AND subject_type = $2 AND subject = $3;

-- name: DueRetrievalItems :many
-- limit_count = 0 means unlimited (NULLIF makes LIMIT NULL) — the same
-- affordance ListVocabularyActivationCandidates documents.
SELECT r.identity_id, r.subject_type, r.subject, r.successes, r.failures, r.last_seen, r.due_at, r.interval_seconds, r.confidence
FROM retrieval_items r
LEFT JOIN vocabulary_items v
       ON r.subject_type = 'expression'
      AND v.identity_id = r.identity_id
      AND v.expression = r.subject
WHERE r.identity_id = $1 AND r.due_at <= $2
  AND (v.identity_id IS NULL OR v.deleted_at IS NULL)
ORDER BY r.due_at ASC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);

-- name: ListRetrievalItems :many
-- limit_count = 0 means unlimited, same as DueRetrievalItems above.
SELECT r.identity_id, r.subject_type, r.subject, r.successes, r.failures, r.last_seen, r.due_at, r.interval_seconds, r.confidence
FROM retrieval_items r
LEFT JOIN vocabulary_items v
       ON r.subject_type = 'expression'
      AND v.identity_id = r.identity_id
      AND v.expression = r.subject
WHERE r.identity_id = $1
  AND (v.identity_id IS NULL OR v.deleted_at IS NULL)
ORDER BY r.due_at ASC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);
