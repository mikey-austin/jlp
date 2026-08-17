-- Soft delete, session cascade (Phase 4 Task D) — see the header
-- comment in db/queries/documents.sql for why conversations and
-- conversation_turns have no deleted_at of their own and instead test
-- the owning session's, always spelled the same way.

-- name: GetConversationBySession :one
SELECT id, session_id, identity_id, created_at, updated_at
FROM conversations
WHERE conversations.session_id = $1 AND conversations.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = conversations.session_id AND s.deleted_at IS NULL);

-- name: InsertConversation :one
-- Guarded for the same reason InsertDocument is: GetOrCreateForSession
-- falls through to this whenever the Get above finds nothing, which now
-- includes "the session was deleted", and conversations.session_id is
-- UNIQUE (00021 migration) — so an unguarded insert would be a 500
-- rather than a clean miss. Zero rows surfaces as pgx.ErrNoRows, mapped
-- to storage.ErrNotFound by the adapter.
INSERT INTO conversations (id, session_id, identity_id, created_at, updated_at)
SELECT $1, $2, $3, $4, $5
WHERE EXISTS (SELECT 1 FROM sessions s WHERE s.id = $2 AND s.deleted_at IS NULL)
RETURNING id, session_id, identity_id, created_at, updated_at;

-- name: InsertConversationTurn :execrows
-- Identity-scoped via a join to conversations — the same "never trust
-- the caller, check via a join" pattern db/queries/lessons.sql's
-- InsertLessonObservation uses: nothing is written unless
-- conversation_id actually belongs to identity_id. :execrows lets the
-- caller tell "wrote" from "no such conversation for this identity"
-- apart — zero rows affected maps to storage.ErrNotFound. The
-- deleted_at test rides along on the same sub-select: a deleted
-- session's conversation must not keep accepting turns.
INSERT INTO conversation_turns (id, conversation_id, position, learner_text, reply, reply_en, followup, corrections, ai_request_id, created_at)
SELECT $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
WHERE EXISTS (
    SELECT 1 FROM conversations c
    JOIN sessions s ON s.id = c.session_id
    WHERE c.id = $2 AND c.identity_id = $11 AND s.deleted_at IS NULL
);

-- name: ListConversationTurns :many
-- Scoped via the same join, oldest first (Position ascending, t.created_at
-- ASC as a tiebreak — Finding I-5: position is computed client-side with
-- no DB-level dedup beyond the 00022 migration's UNIQUE(conversation_id,
-- position), so two concurrent turns are still ordering-ambiguous by
-- position alone if a duplicate somehow lands; created_at makes the
-- order deterministic instead of database/driver-dependent). c.session_id
-- is selected alongside conversation_turns' own columns so
-- fromConversationTurnRow (postgres/conversations.go) can populate
-- storage.ConversationTurn.SessionID on the read path — conversation_turns
-- itself carries no session_id column (see the 00021 migration), only
-- conversation_id, so this join is the only way back to it.
SELECT t.id, t.conversation_id, t.position, t.learner_text, t.reply, t.reply_en, t.followup, t.corrections, t.ai_request_id, t.created_at, c.session_id
FROM conversation_turns t
JOIN conversations c ON t.conversation_id = c.id
WHERE t.conversation_id = $1 AND c.identity_id = $2
  AND EXISTS (SELECT 1 FROM sessions s WHERE s.id = c.session_id AND s.deleted_at IS NULL)
ORDER BY t.position ASC, t.created_at ASC;
