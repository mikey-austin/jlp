-- name: UpsertAIRating :execrows
-- One rating per (ai_request_id, identity_id): rating the same request
-- again replaces the previous value and timestamp rather than adding a
-- second row, per the UNIQUE constraint in 00007_ai_ratings.sql. The id
-- of an existing row is left untouched by the conflict.
--
-- The WHERE EXISTS clause verifies ai_request_id actually belongs to
-- identity_id before writing anything — the same "never trust the
-- caller, check via a join" shape UpdateCorrectionStatus
-- (feedback.sql) uses — so one identity can never attach a rating to
-- another identity's AI request by guessing/observing its id.
-- :execrows lets the caller tell "wrote" from "no such request for
-- this identity" apart: zero rows affected means the latter.
INSERT INTO ai_ratings (id, ai_request_id, identity_id, rating, created_at)
SELECT $1, $2, $3, $4, $5
WHERE EXISTS (SELECT 1 FROM ai_requests req WHERE req.id = $2 AND req.identity_id = $3)
ON CONFLICT (ai_request_id, identity_id)
DO UPDATE SET rating = EXCLUDED.rating, created_at = EXCLUDED.created_at;

-- name: RatingsForRequests :many
-- Scoped by identity_id first: a caller can never read another
-- identity's rating of a request, even one it names explicitly.
SELECT ai_request_id, rating
FROM ai_ratings
WHERE identity_id = $1 AND ai_request_id = ANY(sqlc.arg(request_ids)::uuid[]);
