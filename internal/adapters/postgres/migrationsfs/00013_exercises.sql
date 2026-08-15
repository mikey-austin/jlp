-- +goose Up
CREATE TABLE exercises (
    id           uuid PRIMARY KEY,
    identity_id  text NOT NULL REFERENCES identities(id),
    session_id   uuid REFERENCES sessions(id),
    concept_slug text NOT NULL DEFAULT '',
    type         text NOT NULL,
    payload      jsonb NOT NULL,
    created_at   timestamptz NOT NULL
);
CREATE INDEX exercises_identity_created_idx ON exercises (identity_id, created_at DESC);

CREATE TABLE exercise_attempts (
    id          uuid PRIMARY KEY,
    exercise_id uuid NOT NULL REFERENCES exercises(id),
    response    text NOT NULL,
    correct     boolean,
    score       int,
    feedback    jsonb NOT NULL DEFAULT '{}',
    confidence  int,
    created_at  timestamptz NOT NULL,
    CONSTRAINT exercise_attempts_confidence_range CHECK (confidence IS NULL OR confidence BETWEEN 1 AND 5)
);
CREATE INDEX exercise_attempts_exercise_idx ON exercise_attempts (exercise_id);

-- +goose Down
DROP TABLE exercise_attempts;
DROP TABLE exercises;
