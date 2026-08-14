-- +goose Up
CREATE TABLE ai_ratings (
    id            uuid PRIMARY KEY,
    ai_request_id uuid NOT NULL REFERENCES ai_requests(id),
    identity_id   text NOT NULL REFERENCES identities(id),
    rating        int NOT NULL CHECK (rating BETWEEN 1 AND 5),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (ai_request_id, identity_id)
);
CREATE INDEX ai_ratings_identity_idx ON ai_ratings (identity_id);

-- +goose Down
DROP TABLE ai_ratings;
