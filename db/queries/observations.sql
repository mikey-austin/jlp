-- name: UpsertObservation :exec
INSERT INTO learner_observations (id, identity_id, kind, subject_type, subject, confidence, evidence, first_seen, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
ON CONFLICT (identity_id, subject_type, subject) DO UPDATE SET
    kind       = EXCLUDED.kind,
    confidence = EXCLUDED.confidence,
    evidence   = EXCLUDED.evidence,
    updated_at = EXCLUDED.updated_at;

-- name: ListObservations :many
SELECT id, identity_id, kind, subject_type, subject, confidence, evidence, first_seen, updated_at
FROM learner_observations
WHERE identity_id = $1
ORDER BY updated_at DESC;

-- name: DeleteObservations :exec
DELETE FROM learner_observations WHERE identity_id = $1;
