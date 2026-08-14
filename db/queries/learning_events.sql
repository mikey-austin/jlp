-- name: AppendLearningEvent :exec
INSERT INTO learning_events (id, identity_id, session_id, type, subject, evidence, occurred_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListRecentLearningEvents :many
SELECT id, identity_id, session_id, type, subject, evidence, occurred_at
FROM learning_events
WHERE identity_id = $1 AND ($2::uuid IS NULL OR session_id = $2)
ORDER BY occurred_at DESC
LIMIT $3;

-- name: ListAllLearningEvents :many
SELECT id, identity_id, session_id, type, subject, evidence, occurred_at
FROM learning_events
WHERE identity_id = $1
ORDER BY occurred_at ASC;
