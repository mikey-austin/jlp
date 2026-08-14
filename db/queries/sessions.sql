-- name: CreateSession :exec
INSERT INTO sessions (id, identity_id, title, purpose, profile, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetSession :one
SELECT id, identity_id, title, purpose, profile, created_at, updated_at
FROM sessions
WHERE id = $1 AND identity_id = $2;

-- name: ListSessions :many
SELECT id, identity_id, title, purpose, profile, created_at, updated_at
FROM sessions
WHERE identity_id = $1
ORDER BY updated_at DESC;
