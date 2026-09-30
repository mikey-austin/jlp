-- name: UpsertReadingArticle :one
-- Idempotent ingestion keyed on (identity_id, content_hash). A repeat
-- submission returns the EXISTING live row untouched; the no-op update
-- exists only so RETURNING yields it. A soft-deleted match never reaches
-- here: the repository purges it first, so re-importing a deleted
-- article starts fresh instead of resurrecting the old one. inserted is
-- true only for a genuinely new row (xmax = 0 is postgres' marker for
-- "this row version was inserted, not updated").
INSERT INTO reading_articles (id, identity_id, source_url, source_name, title, author, published_at, paragraphs, content_hash, created_at, original_language, original_title, original_paragraphs)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
ON CONFLICT (identity_id, content_hash) DO UPDATE SET identity_id = EXCLUDED.identity_id
RETURNING id, identity_id, source_url, source_name, title, author, published_at, paragraphs, content_hash, created_at, original_language, original_title, original_paragraphs, (xmax = 0)::boolean AS inserted;

-- name: GetReadingArticle :one
SELECT id, identity_id, source_url, source_name, title, author, published_at, paragraphs, content_hash, created_at, original_language, original_title, original_paragraphs
FROM reading_articles
WHERE id = $1 AND identity_id = $2 AND deleted_at IS NULL;

-- name: SaveReadingTranslation :execrows
-- The worker's write after translating: not identity-scoped, like the
-- edition claim it runs under.
UPDATE reading_articles SET title = $2, paragraphs = $3, original_language = $4, original_title = $5, original_paragraphs = $6
WHERE id = $1;

-- name: InsertReadingEdition :exec
INSERT INTO reading_editions (id, article_id, identity_id, status, prompt_name, prompt_version, schema_name, deliver_when_ready, next_attempt_at, created_at, updated_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- name: LatestReadingEdition :one
-- The newest edition of an article made with one prompt version — what
-- Submit consults to decide whether a repeat submission needs any new
-- work at all.
SELECT e.id, e.article_id, e.identity_id, e.status, e.prompt_name, e.prompt_version, e.schema_name, e.lesson, e.ai_request_id, e.attempts, e.last_error, e.deliver_when_ready, e.created_at, e.updated_at
FROM reading_editions e
WHERE e.article_id = $1 AND e.identity_id = $2 AND e.prompt_name = $3 AND e.prompt_version = $4
ORDER BY e.created_at DESC
LIMIT 1;

-- name: GetReadingEdition :one
-- Identity-scoped, and hidden once its article is soft-deleted: an
-- edition is only reachable through an article the learner can see.
SELECT e.id, e.article_id, e.identity_id, e.status, e.prompt_name, e.prompt_version, e.schema_name, e.lesson, e.ai_request_id, e.attempts, e.last_error, e.deliver_when_ready, e.created_at, e.updated_at
FROM reading_editions e
JOIN reading_articles a ON a.id = e.article_id
WHERE e.id = $1 AND e.identity_id = $2 AND a.deleted_at IS NULL;

-- name: ListReadingEditions :many
-- /reading's list: one row per visible article — its newest edition,
-- plus that edition's newest delivery status (empty when never sent).
SELECT DISTINCT ON (a.id)
    e.id AS edition_id, e.status, e.last_error, e.created_at AS edition_created_at,
    a.id AS article_id, a.title, a.source_name, a.source_url, a.published_at, a.created_at AS article_created_at,
    COALESCE((SELECT d.status FROM reading_deliveries d WHERE d.edition_id = e.id ORDER BY d.created_at DESC LIMIT 1), '')::text AS delivery_status
FROM reading_articles a
JOIN reading_editions e ON e.article_id = a.id
WHERE a.identity_id = $1 AND a.deleted_at IS NULL
ORDER BY a.id, e.created_at DESC;

-- name: ClaimDueReadingEdition :one
-- The analysis worker's claim: the oldest edition that is due (pending
-- and past its backoff) or stuck (claimed by a worker that died more
-- than a stale interval ago), flipped to analysing and counted as an
-- attempt in the same statement. FOR UPDATE SKIP LOCKED lets several
-- app replicas run workers without ever claiming the same row.
UPDATE reading_editions SET status = 'analysing', claimed_at = sqlc.arg(now)::timestamptz, attempts = attempts + 1, updated_at = sqlc.arg(now)::timestamptz
WHERE id = (
    SELECT c.id FROM reading_editions c
    WHERE (c.status = 'pending' AND c.next_attempt_at <= sqlc.arg(now)::timestamptz)
       OR (c.status = 'analysing' AND c.claimed_at < sqlc.arg(stale_before)::timestamptz)
    ORDER BY c.next_attempt_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, article_id, identity_id, status, prompt_name, prompt_version, schema_name, lesson, ai_request_id, attempts, last_error, deliver_when_ready, created_at, updated_at, claimed_at;

-- name: CompleteReadingEdition :execrows
-- Only an edition still in analysing can complete: a stale worker whose
-- claim was taken over must not overwrite the newer attempt's outcome.
UPDATE reading_editions SET status = 'ready', lesson = $2, ai_request_id = $3, last_error = '', claimed_at = NULL, updated_at = $4
WHERE id = $1 AND status = 'analysing' AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: FailReadingEditionAttempt :execrows
-- A failed attempt goes back to pending with a backoff, or to failed
-- when retry is false. Same claim guard as CompleteReadingEdition.
UPDATE reading_editions SET
    status = CASE WHEN sqlc.arg(retry)::boolean THEN 'pending' ELSE 'failed' END,
    last_error = $2, next_attempt_at = $3, claimed_at = NULL, updated_at = $4
WHERE id = $1 AND status = 'analysing' AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: InsertReadingDelivery :one
-- Queues a delivery unless one for the same edition and destination is
-- already in flight (reading_deliveries_inflight_uq), in which case no
-- row comes back and the caller reads the in-flight one instead.
INSERT INTO reading_deliveries (id, edition_id, identity_id, destination, status, next_attempt_at, created_at)
VALUES ($1, $2, $3, $4, 'pending', $5, $5)
ON CONFLICT (edition_id, destination) WHERE status IN ('pending', 'sending') DO NOTHING
RETURNING id, edition_id, identity_id, destination, status, attempts, last_error, created_at, sent_at;

-- name: InFlightReadingDelivery :one
SELECT id, edition_id, identity_id, destination, status, attempts, last_error, created_at, sent_at
FROM reading_deliveries
WHERE edition_id = $1 AND destination = $2 AND status IN ('pending', 'sending');

-- name: ListReadingDeliveries :many
SELECT id, edition_id, identity_id, destination, status, attempts, last_error, created_at, sent_at
FROM reading_deliveries
WHERE edition_id = $1 AND identity_id = $2
ORDER BY created_at DESC;

-- name: ClaimDueReadingDelivery :one
-- Same claim shape as ClaimDueReadingEdition.
UPDATE reading_deliveries SET status = 'sending', claimed_at = sqlc.arg(now)::timestamptz, attempts = attempts + 1
WHERE id = (
    SELECT c.id FROM reading_deliveries c
    WHERE (c.status = 'pending' AND c.next_attempt_at <= sqlc.arg(now)::timestamptz)
       OR (c.status = 'sending' AND c.claimed_at < sqlc.arg(stale_before)::timestamptz)
    ORDER BY c.next_attempt_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, edition_id, identity_id, destination, status, attempts, last_error, created_at, sent_at, claimed_at;

-- name: MarkReadingDeliverySent :execrows
UPDATE reading_deliveries SET status = 'sent', sent_at = $2, last_error = '', claimed_at = NULL
WHERE id = $1 AND status = 'sending' AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: FailReadingDeliveryAttempt :execrows
UPDATE reading_deliveries SET
    status = CASE WHEN sqlc.arg(retry)::boolean THEN 'pending' ELSE 'failed' END,
    last_error = $2, next_attempt_at = $3, claimed_at = NULL
WHERE id = $1 AND status = 'sending' AND claimed_at = sqlc.arg(claimed_at)::timestamptz;

-- name: SoftDeleteReadingArticle :execrows
-- Same identity-scoped, idempotent, non-destructive shape as
-- SoftDeleteLesson (db/queries/lessons.sql).
UPDATE reading_articles SET deleted_at = COALESCE(deleted_at, sqlc.arg(at)::timestamptz)
WHERE id = $1 AND identity_id = $2;

-- name: RestoreReadingArticle :execrows
UPDATE reading_articles SET deleted_at = NULL
WHERE id = $1 AND identity_id = $2;

-- AttachFigures is not a sqlc query: sqlc cannot type a multi-array
-- unnest, so the repository runs that insert through the pool (see
-- attachFiguresSQL in adapters/postgres/reading.go).

-- name: ListReadingFigures :many
SELECT f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256
FROM reading_article_figures f
JOIN reading_articles a ON a.id = f.article_id
WHERE f.article_id = $1 AND a.identity_id = $2 AND a.deleted_at IS NULL
ORDER BY f.ordinal;

-- name: GetReadingFigure :one
SELECT f.ordinal, f.after_paragraph, f.caption, f.alt, f.is_lead, f.in_text, f.media_type, f.width, f.height, f.sha256, f.data
FROM reading_article_figures f
JOIN reading_articles a ON a.id = f.article_id
WHERE f.article_id = $1 AND a.identity_id = $2 AND a.deleted_at IS NULL AND f.ordinal = $3;
