-- +goose Up
CREATE TABLE vocabulary_items (
    id                     uuid PRIMARY KEY,
    identity_id            text NOT NULL REFERENCES identities(id),
    expression             text NOT NULL,
    reading                text NOT NULL DEFAULT '',
    meaning                text NOT NULL DEFAULT '',
    kind                   text NOT NULL DEFAULT 'word',
    jlpt_level             int NOT NULL DEFAULT 0,
    source                 text NOT NULL DEFAULT '',
    lookups                int NOT NULL DEFAULT 0,
    productions            int NOT NULL DEFAULT 0,
    successful_productions int NOT NULL DEFAULT 0,
    first_seen             timestamptz NOT NULL,
    last_event             timestamptz NOT NULL,
    UNIQUE (identity_id, expression)
);

CREATE TABLE vocabulary_events (
    id              uuid PRIMARY KEY,
    identity_id     text NOT NULL REFERENCES identities(id),
    item_id         uuid NOT NULL REFERENCES vocabulary_items(id),
    type            text NOT NULL,
    payload         jsonb NOT NULL DEFAULT '{}',
    client_event_id text,
    occurred_at     timestamptz NOT NULL
);
-- Partial: client_event_id is only sent by clients that want retry
-- idempotency (see storage.VocabularyRepository.UpsertOnLookup's doc
-- comment); rows with no client_event_id must never collide with each
-- other just because they're all NULL.
CREATE UNIQUE INDEX vocabulary_events_identity_client_event_idx
    ON vocabulary_events (identity_id, client_event_id)
    WHERE client_event_id IS NOT NULL;

-- +goose Down
DROP TABLE vocabulary_events;
DROP TABLE vocabulary_items;
