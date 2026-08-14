-- +goose Up
CREATE TABLE learner_priorities (
    identity_id  text NOT NULL REFERENCES identities(id),
    subject_type text NOT NULL,
    subject      text NOT NULL,
    score        float8 NOT NULL,
    reason       text NOT NULL,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (identity_id, subject_type, subject)
);
CREATE INDEX learner_priorities_identity_score_idx ON learner_priorities (identity_id, score DESC);

-- +goose Down
DROP TABLE learner_priorities;
