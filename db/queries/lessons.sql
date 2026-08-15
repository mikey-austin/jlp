-- name: InsertLesson :exec
INSERT INTO lessons (id, identity_id, plan, status, created_at)
VALUES ($1, $2, $3, $4, $5);

-- name: ListLessons :many
SELECT id, identity_id, plan, status, created_at, completed_at
FROM lessons
WHERE identity_id = $1
ORDER BY created_at DESC;

-- name: GetLesson :one
SELECT id, identity_id, plan, status, created_at, completed_at
FROM lessons
WHERE id = $1 AND identity_id = $2;

-- name: CompleteLesson :one
UPDATE lessons SET status = 'completed', completed_at = $3
WHERE id = $1 AND identity_id = $2
RETURNING id, identity_id, plan, status, created_at, completed_at;

-- name: InsertLessonObservation :execrows
-- Identity check via a join to lessons itself, the same "never trust
-- the caller, check via a join" shape UpsertAIRating (ai_ratings.sql)
-- uses: nothing is written unless lesson_id actually belongs to
-- identity_id. :execrows lets the caller (postgres/lessons.go) tell
-- "wrote" from "no such lesson for this identity" apart — zero rows
-- affected means the latter, mapped to storage.ErrNotFound.
INSERT INTO lesson_observations (id, lesson_id, author, notes, subjects, created_at)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM lessons l WHERE l.id = $2 AND l.identity_id = $7);

-- name: ListLessonObservations :many
-- Scoped via the same join, but filters rather than errors on a
-- mismatch — see storage.LessonRepository.Observations' doc comment
-- for why (the caller always Gets the lesson first, which does error).
SELECT o.id, o.lesson_id, o.author, o.notes, o.subjects, o.created_at
FROM lesson_observations o
JOIN lessons l ON o.lesson_id = l.id
WHERE o.lesson_id = $1 AND l.identity_id = $2
ORDER BY o.created_at DESC;
