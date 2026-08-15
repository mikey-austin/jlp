-- name: InsertFeedbackRequest :exec
INSERT INTO feedback_requests (
    id, identity_id, session_id, document_id,
    selection_start, selection_end, selection_text, corrected_text,
    ai_request_id, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: InsertCorrection :exec
INSERT INTO corrections (
    id, feedback_request_id, position, original, replacement,
    type, severity, explanation_ja, explanation_en, hint_ja, hint_en, status, created_at
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13);

-- name: UpdateCorrectionStatus :one
UPDATE corrections c SET status = $3
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
          c.status, c.attempts, c.confidence, c.revealed, f.session_id;

-- name: InsertCorrectionConcept :exec
INSERT INTO correction_concepts (correction_id, concept_slug, resolved)
VALUES ($1, $2, $3)
ON CONFLICT (correction_id, concept_slug) DO NOTHING;

-- name: GetCorrectionConcepts :many
SELECT concept_slug
FROM correction_concepts
WHERE correction_id = $1 AND resolved
ORDER BY concept_slug;

-- name: RetryCorrection :one
-- Atomic "increment attempts, and accept if (and only if) this attempt
-- exactly matches replacement" — see storage.FeedbackRepository's doc
-- comment on why this is one UPDATE (a single, non-racy statement, with
-- the SQL '=' comparison itself deciding correctness) rather than a
-- read-then-write pair. status = 'presented' in the WHERE clause is
-- what confines retries to corrections still awaiting resolution.
UPDATE corrections c SET
    attempts = c.attempts + 1,
    status = CASE WHEN $3 = c.replacement THEN 'accepted' ELSE c.status END
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2 AND c.status = 'presented'
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
          c.status, c.attempts, c.confidence, c.revealed, f.session_id;

-- name: RevealCorrection :one
UPDATE corrections c SET revealed = true
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2 AND c.status = 'presented'
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
          c.status, c.attempts, c.confidence, c.revealed, f.session_id;

-- name: RecordConfidence :one
UPDATE corrections c SET confidence = $3
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
          c.status, c.attempts, c.confidence, c.revealed, f.session_id;
