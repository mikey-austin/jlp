-- name: DeletePriorities :exec
DELETE FROM learner_priorities WHERE identity_id = $1;

-- name: InsertPriority :exec
INSERT INTO learner_priorities (identity_id, subject_type, subject, score, reason, updated_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: TopPriorities :many
SELECT identity_id, subject_type, subject, score, reason, updated_at
FROM learner_priorities
WHERE identity_id = $1
ORDER BY score DESC
LIMIT $2;
