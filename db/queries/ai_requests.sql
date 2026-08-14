-- name: InsertAIRequest :exec
INSERT INTO ai_requests (
    id, identity_id, session_id, capability, provider, model,
    prompt_name, prompt_version, latency_ms, input_tokens, output_tokens,
    cost_usd, success, error, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: ListAIRequests :many
SELECT id, identity_id, session_id, capability, provider, model,
       prompt_name, prompt_version, latency_ms, input_tokens, output_tokens,
       cost_usd, success, error, created_at
FROM ai_requests
WHERE identity_id = $1
ORDER BY created_at DESC
LIMIT $2;
