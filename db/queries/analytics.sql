-- name: CountRunesWritten :one
-- char_length counts Postgres code points, which matches Go's rune
-- count for Japanese text (unlike octet_length/byte length).
SELECT COALESCE(SUM(char_length(content)), 0)::int FROM documents WHERE identity_id = $1;

-- name: CountSessions :one
SELECT COUNT(*)::int FROM sessions WHERE identity_id = $1;

-- name: CountFeedbackRequests :one
SELECT COUNT(*)::int FROM feedback_requests WHERE identity_id = $1;

-- name: CountCorrectionsByStatus :many
-- corrections has no identity_id of its own; scope through the owning
-- feedback_requests row, same join every other identity-scoped
-- corrections query in this codebase uses.
SELECT c.status, COUNT(*)::int AS count
FROM corrections c
JOIN feedback_requests f ON f.id = c.feedback_request_id
WHERE f.identity_id = $1
GROUP BY c.status;

-- name: TopErrorTypes :many
-- Secondary "type ASC" tiebreak keeps the top-5 stable across requests
-- when two error types are presented equally often — Postgres makes no
-- ordering guarantee among GROUP BY ties otherwise.
SELECT c.type, COUNT(*)::int AS count
FROM corrections c
JOIN feedback_requests f ON f.id = c.feedback_request_id
WHERE f.identity_id = $1
GROUP BY c.type
ORDER BY count(*) DESC, c.type ASC
LIMIT 5;
