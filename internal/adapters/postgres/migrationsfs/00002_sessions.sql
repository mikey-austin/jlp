-- +goose Up
CREATE TABLE sessions (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    title       text NOT NULL,
    purpose     text NOT NULL DEFAULT '',
    profile     jsonb NOT NULL DEFAULT '{}',
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sessions_identity_idx ON sessions (identity_id, updated_at DESC);

-- +goose Down
DROP TABLE sessions;
