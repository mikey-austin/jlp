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

-- name: GetCorrection :one
-- Identity-scoped read, same join as UpdateCorrectionStatus: a
-- correction that exists but belongs to another identity's feedback
-- misses exactly like one that doesn't exist at all.
SELECT c.id, c.feedback_request_id, c.position, c.original, c.replacement,
       c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
       c.status, c.attempts, c.confidence, c.revealed, f.session_id
FROM corrections c
JOIN feedback_requests f ON c.feedback_request_id = f.id
WHERE c.id = $1 AND f.identity_id = $2;

-- name: RecentCorrections :many
-- Identity-scoped, unfiltered by status (Phase 3 Task 4, PRD §18): a
-- lesson guide benefits from seeing what was corrected regardless of
-- whether the learner has since accepted, rejected, or not yet
-- responded — same join UpdateCorrectionStatus/GetCorrection use, newest
-- first via c.created_at.
SELECT c.id, c.feedback_request_id, c.position, c.original, c.replacement,
       c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
       c.status, c.attempts, c.confidence, c.revealed, f.session_id
FROM corrections c
JOIN feedback_requests f ON c.feedback_request_id = f.id
WHERE f.identity_id = $1
ORDER BY c.created_at DESC
LIMIT $2;

-- name: RecordConfidence :one
UPDATE corrections c SET confidence = $3
FROM feedback_requests f
WHERE c.id = $1 AND c.feedback_request_id = f.id AND f.identity_id = $2
RETURNING c.id, c.feedback_request_id, c.position, c.original, c.replacement,
          c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
          c.status, c.attempts, c.confidence, c.revealed, f.session_id;

-- name: ListFeedbackForSession :many
-- Identity- and session-scoped feedback history (Phase 4 Task W item
-- 2), most-recent-first: one row per feedback_requests entry with its
-- correction count and the ai_requests-joined provider/model that
-- served it (both '' when ai_request_id is NULL — see
-- GetFeedbackRequest below). a.provider/a.model must be listed in
-- GROUP BY explicitly (unlike f's own columns, which Postgres treats as
-- functionally dependent on the f.id primary key) since they come from
-- a joined table.
SELECT f.id, f.selection_text, f.created_at,
       COUNT(c.id) AS correction_count,
       COALESCE(a.provider, '') AS provider,
       COALESCE(a.model, '') AS model
FROM feedback_requests f
LEFT JOIN corrections c ON c.feedback_request_id = f.id
LEFT JOIN ai_requests a ON a.id = f.ai_request_id
WHERE f.identity_id = $1 AND f.session_id = $2
GROUP BY f.id, f.selection_text, f.created_at, a.provider, a.model
ORDER BY f.created_at DESC;

-- name: GetFeedbackRequest :one
-- Identity-scoped read of one feedback_requests row plus the
-- ai_requests row it produced (Phase 4 Task W items 2/3): a LEFT JOIN
-- since ai_request_id is nullable (a fakeai-backed review outside the
-- observability decorator never sets it — see feedback.go's
-- toOptionalUUID).
SELECT f.id, f.selection_text, f.corrected_text, f.created_at,
       COALESCE(a.provider, '') AS provider,
       COALESCE(a.model, '') AS model
FROM feedback_requests f
LEFT JOIN ai_requests a ON a.id = f.ai_request_id
WHERE f.id = $1 AND f.identity_id = $2;

-- name: ListCorrectionsForFeedback :many
-- Every correction for one feedback_request_id, in Position order — NOT
-- separately identity-scoped (see storage.FeedbackRepository.GetFeedback's
-- doc comment: the caller must already have proven ownership of
-- feedback_request_id via GetFeedbackRequest above before calling this).
-- Same column shape as GetCorrection/RecentCorrections so the postgres
-- adapter can reuse buildCorrectionRecord unchanged.
SELECT c.id, c.feedback_request_id, c.position, c.original, c.replacement,
       c.type, c.severity, c.explanation_ja, c.explanation_en, c.hint_ja, c.hint_en,
       c.status, c.attempts, c.confidence, c.revealed, f.session_id
FROM corrections c
JOIN feedback_requests f ON c.feedback_request_id = f.id
WHERE c.feedback_request_id = $1
ORDER BY c.position;
