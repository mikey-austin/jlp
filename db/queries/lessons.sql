-- name: InsertLesson :exec
INSERT INTO lessons (id, identity_id, plan, status, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ListLessons :many
SELECT id, identity_id, plan, status, created_at, completed_at, deleted_at
FROM lessons
WHERE identity_id = $1 AND deleted_at IS NULL
ORDER BY created_at DESC;

-- name: GetLesson :one
SELECT id, identity_id, plan, status, created_at, completed_at, deleted_at
FROM lessons
WHERE id = $1 AND identity_id = $2 AND deleted_at IS NULL;

-- name: CompleteLesson :one
-- Run inside the SAME transaction as InsertLessonObservation below —
-- see postgres/lessons.go's CompleteWithObservation, the ONLY caller of
-- either query: a lesson must never end up "completed" without its
-- triggering observation actually attached, or vice versa. Not called
-- standalone by anything else. The deleted_at test is the same one Get
-- applies: a deleted lesson is invisible to reads, so it must not be
-- completable either — otherwise the tutor's observation would attach
-- to something the learner can no longer see.
UPDATE lessons SET status = 'completed', completed_at = $3
WHERE id = $1 AND identity_id = $2 AND deleted_at IS NULL
RETURNING id, identity_id, plan, status, created_at, completed_at, deleted_at;

-- name: InsertLessonObservation :execrows
-- Run inside the SAME transaction as CompleteLesson above, by
-- postgres/lessons.go's CompleteWithObservation. Identity check via a
-- join to lessons itself, the same "never trust the caller, check via
-- a join" shape UpsertAIRating (ai_ratings.sql) uses: nothing is
-- written unless lesson_id actually belongs to identity_id AND is not
-- soft-deleted. :execrows lets the caller tell "wrote" from "no such
-- lesson for this identity" apart — zero rows affected means the
-- latter, mapped to storage.ErrNotFound (and, since this runs inside
-- the same transaction, rolls back CompleteLesson's status flip too).
INSERT INTO lesson_observations (id, lesson_id, author, notes, subjects, created_at)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM lessons l WHERE l.id = $2 AND l.identity_id = $7 AND l.deleted_at IS NULL);

-- name: ListLessonObservations :many
-- Scoped via the same join, but filters rather than errors on a
-- mismatch — see storage.LessonRepository.Observations' doc comment
-- for why (the caller always Gets the lesson first, which does error).
-- A soft-deleted lesson's observations are filtered here for the same
-- reason: the lesson itself is unreachable, so its notes must be too.
SELECT o.id, o.lesson_id, o.author, o.notes, o.subjects, o.created_at
FROM lesson_observations o
JOIN lessons l ON o.lesson_id = l.id
WHERE o.lesson_id = $1 AND l.identity_id = $2 AND l.deleted_at IS NULL
ORDER BY o.created_at DESC;

-- name: SoftDeleteLesson :execrows
-- The learner's "delete this lesson guide" (Phase 4 Task D). Identity-
-- scoped, idempotent, and non-destructive for exactly the reasons
-- db/queries/sessions.sql's SoftDeleteSession spells out — read that
-- comment; this is the same statement over a different table, on
-- purpose, so there is one shape to understand rather than three.
UPDATE lessons SET deleted_at = COALESCE(deleted_at, sqlc.arg(at)::timestamptz)
WHERE id = $1 AND identity_id = $2;

-- name: RestoreLesson :execrows
-- The way back — see SoftDeleteSession/RestoreSession in
-- db/queries/sessions.sql.
UPDATE lessons SET deleted_at = NULL
WHERE id = $1 AND identity_id = $2;
