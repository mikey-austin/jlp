-- API tokens for non-browser clients. See migration 00027 for why only
-- the hash is stored.
--
-- api_tokens is deliberately NOT in softdelete_guard_test.go's
-- guardedTables: revoked_at is not a learner hiding their own content
-- from every read surface, it is a credential being switched off. The
-- one place that must respect it is authentication, and
-- GetLiveAPITokenByHash below is that place — which is why the
-- predicate is spelled out there rather than left to a caller.

-- name: InsertAPIToken :exec
INSERT INTO api_tokens (id, identity_id, name, token_hash, scopes)
VALUES ($1, $2, $3, $4, $5);

-- GetLiveAPITokenByHash is the authentication lookup, run on every API
-- request. "Live" is the whole point: a revoked token must stop working
-- immediately, so the predicate is here rather than in Go where a future
-- caller could omit it.
-- name: GetLiveAPITokenByHash :one
SELECT id, identity_id, scopes
FROM api_tokens
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: TouchAPITokenLastUsed :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = $1;

-- ListAPITokens shows a learner their own tokens, including revoked
-- ones: "this app's token was revoked on the 3rd" is exactly what you
-- want to see when auditing which reader apps can write to your record.
-- name: ListAPITokens :many
SELECT id, name, scopes, created_at, last_used_at, revoked_at
FROM api_tokens
WHERE identity_id = $1
ORDER BY created_at DESC;

-- RevokeAPIToken is identity-scoped so that knowing a token id is not
-- enough to switch off someone else's client.
-- name: RevokeAPIToken :execrows
UPDATE api_tokens
SET revoked_at = now()
WHERE id = $1 AND identity_id = $2 AND revoked_at IS NULL;
