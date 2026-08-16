-- +goose Up
-- Code review follow-up (Phase 4 Task 8, PRD §66): application/
-- conversation.Service.validSourceEvent's check-then-write ("does this
-- speech.transcribed event exist, is it unused") has a race — two
-- concurrent Say calls carrying the SAME genuine event id can both
-- pass the ListRecent check before either INSERT lands, tagging one
-- speech event onto two conversation.turn events. That breaks "one
-- speech event, one turn", the exact invariant Task 9's spoken-vs-
-- typed analytics depends on.
--
-- A partial unique index makes Postgres itself the enforcer,
-- regardless of process count or a future caller that forgets the
-- application-level check: at most one learning_events row may ever
-- carry a given evidence->>'speech_event_id' value. The index is
-- partial (WHERE evidence ? 'speech_event_id') so it costs nothing for
-- the overwhelming majority of events that never carry this key at
-- all, and NULL/absent never collides with anything.
--
-- application/conversation.Service.Say still runs its own read-side
-- check first (the friendly path: a losing racer downgrades silently
-- to a typed turn rather than surfacing a database error) — this
-- index is the backstop for the narrow window between that check and
-- the write, not a replacement for it.
CREATE UNIQUE INDEX learning_events_speech_event_id_idx
    ON learning_events ((evidence->>'speech_event_id'))
    WHERE evidence ? 'speech_event_id';

-- +goose Down
DROP INDEX learning_events_speech_event_id_idx;
