-- +goose Up
CREATE TABLE learner_observations (
    id          uuid PRIMARY KEY,
    identity_id text NOT NULL REFERENCES identities(id),
    kind        text NOT NULL,
    subject_type text NOT NULL,
    subject     text NOT NULL,
    confidence  float8 NOT NULL,
    evidence    jsonb NOT NULL DEFAULT '{}',
    first_seen  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL,
    UNIQUE (identity_id, subject_type, subject)
);
CREATE INDEX learner_observations_identity_idx ON learner_observations (identity_id);

-- +goose Down
DROP TABLE learner_observations;
