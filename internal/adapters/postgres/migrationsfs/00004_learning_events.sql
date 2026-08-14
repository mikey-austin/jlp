-- +goose Up
CREATE TABLE learning_events (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    session_id  uuid REFERENCES sessions(id),
    type        text NOT NULL,
    subject     text NOT NULL DEFAULT '',
    evidence    jsonb NOT NULL DEFAULT '{}',
    occurred_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX learning_events_identity_time_idx ON learning_events (identity_id, occurred_at DESC);
CREATE INDEX learning_events_type_idx ON learning_events (identity_id, type);

-- +goose Down
DROP TABLE learning_events;
