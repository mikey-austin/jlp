-- name: CreateSession :exec
INSERT INTO sessions (id, identity_id, title, purpose, profile, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetSession :one
SELECT id, identity_id, title, purpose, profile, created_at, updated_at, deleted_at
FROM sessions
WHERE id = $1 AND identity_id = $2 AND deleted_at IS NULL;

-- name: ListSessions :many
SELECT id, identity_id, title, purpose, profile, created_at, updated_at, deleted_at
FROM sessions
WHERE identity_id = $1 AND deleted_at IS NULL
ORDER BY updated_at DESC;

-- name: SoftDeleteSession :execrows
-- The learner's "delete this session" (Phase 4 Task D). Three
-- properties, all carried by this one statement rather than by the
-- caller:
--
--  1. Identity-scoped from the request context, never from the body:
--     identity_id is part of the WHERE, so another identity's session
--     matches zero rows and is reported EXACTLY like an id that does
--     not exist (storage.ErrNotFound) — no existence oracle, and the
--     row is left untouched.
--  2. Idempotent: there is no "AND deleted_at IS NULL" here on
--     purpose. Deleting an already-deleted session still matches its
--     row, so :execrows is 1 and the caller sees success, not an
--     error.
--  3. Non-destructive and stable: COALESCE keeps the ORIGINAL deletion
--     timestamp on a repeat delete, so "when did I delete this" stays
--     answerable, and updated_at is deliberately NOT bumped so a
--     restored session reappears at its real place in the list.
--
-- The documents, feedback requests, corrections and conversation turns
-- hanging off the session are not touched: they disappear from every
-- read because their own queries test this row's deleted_at through an
-- EXISTS sub-select (see db/queries/documents.sql, feedback.sql,
-- conversations.sql), which is also what makes a restore complete.
UPDATE sessions SET deleted_at = COALESCE(deleted_at, sqlc.arg(at)::timestamptz)
WHERE id = $1 AND identity_id = $2;

-- name: RestoreSession :execrows
-- The way back (`jlp restore session <identity> <id>`, and the undo
-- affordance /sessions offers straight after a delete). Same
-- identity-scoping and same idempotence as SoftDeleteSession above:
-- restoring a session that was never deleted is a success, and another
-- identity's session matches zero rows.
UPDATE sessions SET deleted_at = NULL
WHERE id = $1 AND identity_id = $2;
