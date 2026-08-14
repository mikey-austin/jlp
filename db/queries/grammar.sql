-- name: UpsertGrammarConcept :exec
INSERT INTO grammar_concepts (slug, name, jlpt_level, description, examples, related, prerequisites)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (slug) DO UPDATE SET
    name          = EXCLUDED.name,
    jlpt_level    = EXCLUDED.jlpt_level,
    description   = EXCLUDED.description,
    examples      = EXCLUDED.examples,
    related       = EXCLUDED.related,
    prerequisites = EXCLUDED.prerequisites;

-- name: ListGrammarConcepts :many
SELECT slug, name, jlpt_level, description, examples, related, prerequisites
FROM grammar_concepts
ORDER BY jlpt_level DESC, slug;

-- name: GetGrammarConcept :one
SELECT slug, name, jlpt_level, description, examples, related, prerequisites
FROM grammar_concepts
WHERE slug = $1;

-- name: ConceptStats :many
-- Scoped subquery, not a straight LEFT JOIN + WHERE: filtering
-- correction_concepts/corrections/feedback_requests to the caller's
-- identity has to happen INSIDE the joined subquery (oc) so that a
-- concept tagged only by another identity's corrections still
-- produces a row here (encounters=0), rather than a WHERE clause on
-- the outer query dropping the grammar_concepts row entirely because
-- its only correction_concepts match belongs to someone else.
SELECT gc.slug, gc.name, gc.jlpt_level,
       COUNT(oc.correction_id)::int AS encounters,
       COALESCE(MAX(oc.created_at), 'epoch'::timestamptz) AS last_seen
FROM grammar_concepts gc
LEFT JOIN (
    SELECT cc.concept_slug, cc.correction_id, c.created_at
    FROM correction_concepts cc
    JOIN corrections c ON c.id = cc.correction_id
    JOIN feedback_requests f ON f.id = c.feedback_request_id
    WHERE cc.resolved AND f.identity_id = $1
) oc ON oc.concept_slug = gc.slug
GROUP BY gc.slug, gc.name, gc.jlpt_level
ORDER BY encounters DESC, gc.jlpt_level DESC, gc.slug;

-- name: CorrectionsForConcept :many
SELECT c.id, c.feedback_request_id, c.position, c.original, c.replacement,
       c.type, c.severity, c.explanation_ja, c.explanation_en, c.status, f.session_id
FROM corrections c
JOIN correction_concepts cc ON cc.correction_id = c.id AND cc.concept_slug = $2 AND cc.resolved
JOIN feedback_requests f ON f.id = c.feedback_request_id AND f.identity_id = $1
ORDER BY c.created_at DESC
LIMIT $3;
