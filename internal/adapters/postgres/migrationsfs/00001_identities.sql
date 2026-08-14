-- +goose Up
CREATE TABLE identities (
    id           text PRIMARY KEY,
    display_name text NOT NULL,
    attributes   jsonb NOT NULL DEFAULT '{}',
    created_at   timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE identities;
