-- name: InsertExercise :exec
INSERT INTO exercises (id, identity_id, session_id, concept_slug, type, payload, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetExercise :one
SELECT payload FROM exercises WHERE id = $1 AND identity_id = $2;

-- name: InsertExerciseAttempt :exec
INSERT INTO exercise_attempts (id, exercise_id, response, correct, score, feedback, confidence, created_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
