-- +goose Up
CREATE TABLE anki_cards (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    source_type text NOT NULL,
    source_id   text NOT NULL,
    front       text NOT NULL,
    back        text NOT NULL,
    notes       text NOT NULL DEFAULT '',
    status      text NOT NULL DEFAULT 'draft',
    created_at  timestamptz NOT NULL
);
CREATE INDEX anki_cards_identity_idx ON anki_cards (identity_id, created_at DESC);

-- +goose Down
DROP TABLE anki_cards;
