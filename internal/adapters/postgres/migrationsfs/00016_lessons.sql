-- +goose Up
CREATE TABLE lessons (
    id           uuid PRIMARY KEY,
    identity_id  text NOT NULL REFERENCES identities(id),
    plan         jsonb NOT NULL,
    status       text NOT NULL DEFAULT 'prepared',
    created_at   timestamptz NOT NULL,
    completed_at timestamptz
);
CREATE INDEX lessons_identity_idx ON lessons (identity_id, created_at DESC);

CREATE TABLE lesson_observations (
    id         uuid PRIMARY KEY,
    lesson_id  uuid NOT NULL REFERENCES lessons(id),
    author     text NOT NULL,
    notes      text NOT NULL,
    subjects   jsonb NOT NULL DEFAULT '[]',
    created_at timestamptz NOT NULL
);
CREATE INDEX lesson_observations_lesson_idx ON lesson_observations (lesson_id, created_at DESC);

-- +goose Down
DROP TABLE lesson_observations;
DROP TABLE lessons;
