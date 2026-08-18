-- +goose Up
-- /vocabulary listed every word a learner had ever saved, in one page.
-- That was fine at a few dozen and is not at 500: the page is a wall on
-- a phone, and the query's cost grows with the whole catalogue whether
-- or not anyone scrolls that far.
--
-- Cursor (keyset) pagination rather than OFFSET: with OFFSET, page N
-- still makes the database walk the N-1 pages before it, and a word
-- whose last_event moves while you are paging shifts every later page,
-- so rows are silently skipped or repeated. A keyset asks "the next 50
-- strictly after this exact position", which is stable under concurrent
-- edits and costs the same on page 10 as on page 1 — and vocabulary
-- rows DO move: every lookup updates last_event, which is the sort key.
--
-- The tiebreaker is what makes it correct. last_event alone is not
-- unique (a bulk import gives hundreds of rows the same timestamp — the
-- 500-word import that prompted this), so a cursor on last_event only
-- would jump over every row sharing the boundary timestamp. Ordering by
-- (last_event, id) is a total order, so the cursor names exactly one
-- row.
--
-- The index must match that order exactly, or the sort it exists to
-- avoid comes back.
DROP INDEX vocabulary_items_identity_last_event_idx;

CREATE INDEX vocabulary_items_identity_keyset_idx
    ON vocabulary_items (identity_id, last_event DESC, id DESC)
    WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX vocabulary_items_identity_keyset_idx;

CREATE INDEX vocabulary_items_identity_last_event_idx
    ON vocabulary_items (identity_id, last_event DESC)
    WHERE deleted_at IS NULL;
