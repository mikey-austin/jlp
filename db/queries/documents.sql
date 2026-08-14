-- name: GetDocumentBySession :one
SELECT id, session_id, identity_id, content, version, updated_at
FROM documents
WHERE session_id = $1 AND identity_id = $2;

-- name: InsertDocument :one
INSERT INTO documents (id, session_id, identity_id, content, version, updated_at)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, session_id, identity_id, content, version, updated_at;

-- name: GetDocument :one
SELECT id, session_id, identity_id, content, version, updated_at
FROM documents
WHERE id = $1 AND identity_id = $2;

-- name: UpdateDocumentContent :one
UPDATE documents
SET content = $3, version = version + 1, updated_at = now()
WHERE id = $1 AND identity_id = $2
RETURNING *;

-- name: InsertDocumentVersion :exec
INSERT INTO document_versions (document_id, version, content, created_at)
VALUES ($1, $2, $3, $4);

-- name: ListDocumentVersions :many
SELECT dv.id, dv.document_id, dv.version, dv.content, dv.created_at
FROM document_versions dv
JOIN documents d ON d.id = dv.document_id
WHERE dv.document_id = $1 AND d.identity_id = $2
ORDER BY dv.version DESC
LIMIT $3;
