-- +goose Up
-- Finding I-5 (Task 6 fix round): position is computed client-side in
-- application/conversation.Service.Say (len(history)+1), with no
-- database-level dedup. Two Say calls racing for the same conversation
-- (e.g. the learner submitting a second message before the first
-- reply's multi-second AI call returns) could both compute the same
-- position and both insert — corrupting transcript order and, since
-- "delayed" timing's batch boundary is position%3==0, silently
-- skipping or double-releasing a batch. This constraint turns that into
-- a loud INSERT failure instead of a silent duplicate; the app-layer
-- mitigations (ORDER BY tiebreak in ListConversationTurns, disabling
-- the chat input while a turn is in flight — see conversations.sql and
-- workspace.html.tmpl) address the common case, this is the backstop.
ALTER TABLE conversation_turns
    ADD CONSTRAINT conversation_turns_conversation_position_uniq UNIQUE (conversation_id, position);

-- +goose Down
ALTER TABLE conversation_turns
    DROP CONSTRAINT conversation_turns_conversation_position_uniq;
