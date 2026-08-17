-- Soft delete, session cascade (Phase 4 Task D).
--
-- documents and document_versions carry no deleted_at of their own: a
-- document belongs to exactly one session (UNIQUE documents.session_id,
-- 00003 migration), so "is this document deleted?" has one answer —
-- "is its session deleted?". Every query below therefore tests the
-- OWNING SESSION's deleted_at, spelled the same way each time:
--
--     AND EXISTS (SELECT 1 FROM sessions s
--                  WHERE s.id = <docs>.session_id AND s.deleted_at IS NULL)
--
-- EXISTS rather than a JOIN so the projection stays exactly the
-- documents table's own column list and sqlc keeps emitting the plain
-- Document row struct. internal/adapters/postgres/softdelete_guard_test.go
-- fails the build if a new query in this file omits the predicate
-- without an explicit, reasoned exemption.

-- name: GetDocumentBySession :one
SELECT id, session_id, identity_id, content, version, updated_at
FROM documents
WHERE documents.session_id = $1 AND documents.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = documents.session_id AND s.deleted_at IS NULL);

-- name: InsertDocument :one
-- Guarded, not a bare INSERT: postgres/documents.go's
-- GetOrCreateForSession falls through to this the moment
-- GetDocumentBySession finds nothing — which, now that that query
-- filters deleted sessions, includes "the session was deleted and its
-- document is hidden". Without the guard that fallthrough would try to
-- insert a SECOND document for the same session_id and hit the UNIQUE
-- index as a 500 instead of a clean miss. Zero rows here surfaces as
-- pgx.ErrNoRows, which the adapter maps to storage.ErrNotFound.
INSERT INTO documents (id, session_id, identity_id, content, version, updated_at)
SELECT $1, $2, $3, $4, $5, $6
WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = $2 AND s.deleted_at IS NULL)
RETURNING id, session_id, identity_id, content, version, updated_at;

-- name: GetDocument :one
SELECT id, session_id, identity_id, content, version, updated_at
FROM documents
WHERE documents.id = $1 AND documents.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = documents.session_id AND s.deleted_at IS NULL);

-- name: UpdateDocumentContent :one
-- The predicate matters more here than on any read: POST /documents/{id}
-- autosaves by document id alone and never resolves the session, so
-- without this a deleted session's document would stay fully writable
-- from a stale tab. Zero rows maps to storage.ErrNotFound in the
-- adapter, the same miss a wrong identity already produces.
UPDATE documents
SET content = $3, version = version + 1, updated_at = now()
WHERE documents.id = $1 AND documents.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = documents.session_id AND s.deleted_at IS NULL)
RETURNING *;

-- name: InsertDocumentVersion :exec
INSERT INTO document_versions (document_id, version, content, created_at)
VALUES ($1, $2, $3, $4);

-- name: ListDocumentVersions :many
SELECT dv.id, dv.document_id, dv.version, dv.content, dv.created_at
FROM document_versions dv
JOIN documents d ON d.id = dv.document_id
WHERE dv.document_id = $1 AND d.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = d.session_id AND s.deleted_at IS NULL)
ORDER BY dv.version DESC
LIMIT $3;
