-- name: VocabFunnel :one
-- Raw sums across the identity's whole vocabulary bank: lookups,
-- productions, and successful productions — the three stages of
-- storage.VocabFunnel. COALESCE covers an identity with no vocabulary
-- rows at all (SUM of zero rows is NULL, not 0).
SELECT COALESCE(SUM(lookups), 0)::int AS looked_up,
       COALESCE(SUM(productions), 0)::int AS produced,
       COALESCE(SUM(successful_productions), 0)::int AS produced_correctly
FROM vocabulary_items
WHERE identity_id = $1;

-- name: TopWeaknessSubjects :many
-- The (subject_type, subject) pairs WeaknessTrends charts: identity's
-- top 3 live weaknesses (learner_observations.kind='weakness'), most
-- confident first with recency as the tiebreak, subject ASC beneath
-- that so ties are stable across requests (mirrors TopErrorTypes'
-- tiebreak rationale in analytics.sql).
SELECT subject, subject_type
FROM learner_observations
WHERE identity_id = $1 AND kind = 'weakness'
ORDER BY confidence DESC, updated_at DESC, subject ASC
LIMIT 3;

-- name: SubjectOccurrencesByWeek :many
-- Weekly occurrence counts for one (subject_type, subject) pair from
-- $3/$4, over learning_events from $2 (inclusive) to now. Mirrors
-- application/learnermodel's classify()/eventTypeFor() event-type
-- mapping used for weakness detection:
--   - subject_type 'concept': occurrences are
--     grammar.concept.encountered events whose subject IS the concept
--     slug directly.
--   - subject_type 'correction-type': occurrences are
--     correction.presented events, but learning_events.subject holds
--     the CORRECTION ID for those (see learnermodel.dedupKey's doc
--     comment) — the correction type text instead lives in
--     evidence->>'type'.
-- date_trunc runs against occurred_at converted to plain UTC (AT TIME
-- ZONE 'UTC') rather than relying on the session's timezone setting,
-- so week buckets are deterministic regardless of how the DB
-- connection is configured — the Go caller computes its own zero-fill
-- week boundaries the identical way (see isoWeekStarts).
SELECT date_trunc('week', occurred_at AT TIME ZONE 'UTC')::date AS week_start,
       COUNT(*)::int AS count
FROM learning_events
WHERE identity_id = $1
  AND occurred_at >= $2
  AND (
    ($3::text = 'concept' AND type = 'grammar.concept.encountered' AND subject = $4)
    OR ($3::text = 'correction-type' AND type = 'correction.presented' AND evidence ->> 'type' = $4)
  )
GROUP BY week_start
ORDER BY week_start;

-- name: ConfidenceCalibration :many
-- Per-confidence-level (1..5) attempt count and correct count, from
-- exercise_attempts rows that actually carry a confidence value —
-- confidence IS NULL rows (no self-rating given) never contribute.
-- Raw counts only, per the codebase's "SQL returns raw data, Go
-- derives" convention (see analytics.go's Statistics doc comment):
-- application/analytics.Service computes CorrectRate from
-- Attempts/Corrects, the same way it computes AcceptanceRate from
-- Statistics' raw correction counts. An identity with no
-- confidence-rated attempts at all simply gets zero rows back, which
-- the template renders as an explicit empty state.
SELECT ea.confidence AS confidence,
       COUNT(*)::int AS attempts,
       (COUNT(*) FILTER (WHERE ea.correct))::int AS corrects
FROM exercise_attempts ea
JOIN exercises e ON e.id = ea.exercise_id
WHERE e.identity_id = $1 AND ea.confidence IS NOT NULL
GROUP BY ea.confidence
ORDER BY ea.confidence;

-- name: AgentUsageStats :many
-- Per-agent request volume, success count, and average latency from
-- ai_requests — agent is '' for rows written before the column existed
-- (see the 00017 migration); the '' bucket is returned like any other
-- and it's the HTTP layer's job to label it for display (see
-- learner.html.tmpl's 未分類 fallback). Raw counts only (see
-- ConfidenceCalibration's doc comment above) — SuccessRate is derived
-- in application/analytics.Service from Requests/Successes.
SELECT agent,
       COUNT(*)::int AS requests,
       (COUNT(*) FILTER (WHERE success))::int AS successes,
       ROUND(AVG(latency_ms))::int AS avg_latency_ms
FROM ai_requests
WHERE identity_id = $1
GROUP BY agent
ORDER BY requests DESC, agent ASC;

-- name: LearningEventsByType :many
SELECT type, COUNT(*)::int AS count
FROM learning_events
WHERE identity_id = $1
GROUP BY type
ORDER BY type;

-- name: CountAIRequestsForIdentity :one
SELECT COUNT(*)::int FROM ai_requests WHERE identity_id = $1;
