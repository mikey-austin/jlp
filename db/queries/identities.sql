-- name: UpsertIdentity :exec
INSERT INTO identities (id, display_name, attributes)
VALUES ($1, $2, $3)
ON CONFLICT (id) DO UPDATE SET display_name = EXCLUDED.display_name, attributes = EXCLUDED.attributes;

-- name: GetIdentity :one
SELECT id, display_name, attributes, created_at FROM identities WHERE id = $1;
