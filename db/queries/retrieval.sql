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
SELECT identity_id, subject_type, subject, successes, failures, last_seen, due_at, interval_seconds, confidence
FROM retrieval_items
WHERE identity_id = $1 AND due_at <= $2
ORDER BY due_at ASC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);

-- name: ListRetrievalItems :many
-- limit_count = 0 means unlimited, same as DueRetrievalItems above.
SELECT identity_id, subject_type, subject, successes, failures, last_seen, due_at, interval_seconds, confidence
FROM retrieval_items
WHERE identity_id = $1
ORDER BY due_at ASC
LIMIT NULLIF(sqlc.arg(limit_count)::int, 0);
