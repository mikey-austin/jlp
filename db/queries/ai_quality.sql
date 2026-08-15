-- name: StatsByProvider :many
-- Aggregates ai_requests per (provider, model) for one identity: request
-- count, success rate, average rating (0 when nothing's been rated),
-- average + p95 latency, and total cost. The ai_ratings join is scoped
-- by identity_id on both sides — rat.identity_id = $1, matching
-- req.identity_id = $1 in the WHERE clause — the same "never trust a
-- bare id join" discipline RatingsForRequests (ai_ratings.sql) uses, so
-- a rating can never be double-counted or leak across identities even
-- if the ai_ratings row's ownership invariant were ever violated.
--
-- p95 uses percentile_cont(0.95) WITHIN GROUP, PG's continuous
-- (interpolating) percentile — appropriate for a latency distribution
-- rather than percentile_disc's "nearest actual sample" discrete pick.
-- Both AVG and the percentile are cast through numeric before rounding
-- to an int millisecond value: round(numeric) is always available,
-- unlike round(double precision) on older Postgres, and percentile_cont
-- always returns double precision regardless of the input column's type.
SELECT
    req.provider,
    req.model,
    COUNT(*)::int AS requests,
    ((COUNT(*) FILTER (WHERE req.success))::float8 / COUNT(*)::float8)::float8 AS success_rate,
    COALESCE(AVG(rat.rating), 0)::float8 AS avg_rating,
    ROUND(AVG(req.latency_ms))::int AS avg_latency_ms,
    ROUND(COALESCE(percentile_cont(0.95) WITHIN GROUP (ORDER BY req.latency_ms), 0)::numeric)::int AS p95_latency_ms,
    COALESCE(SUM(req.cost_usd), 0)::float8 AS total_cost_usd
FROM ai_requests req
LEFT JOIN ai_ratings rat ON rat.ai_request_id = req.id AND rat.identity_id = $1
WHERE req.identity_id = $1
GROUP BY req.provider, req.model
ORDER BY req.provider, req.model;

-- name: StatsByPrompt :many
-- Same shape as StatsByProvider but grouped by (prompt_name,
-- prompt_version) instead of (provider, model) — the "quality over
-- prompt version" view PRD §26 asks for: teacher.feedback v2 vs v3,
-- drill.generate v1, etc, each kept as its own row so a prompt
-- rewrite's effect on rating/success rate is visible rather than
-- conflated with an earlier or later version of the same prompt name.
SELECT
    req.prompt_name,
    req.prompt_version,
    COUNT(*)::int AS requests,
    COALESCE(AVG(rat.rating), 0)::float8 AS avg_rating,
    ((COUNT(*) FILTER (WHERE req.success))::float8 / COUNT(*)::float8)::float8 AS success_rate
FROM ai_requests req
LEFT JOIN ai_ratings rat ON rat.ai_request_id = req.id AND rat.identity_id = $1
WHERE req.identity_id = $1
GROUP BY req.prompt_name, req.prompt_version
ORDER BY req.prompt_name, req.prompt_version;
