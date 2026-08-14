-- +goose Up
CREATE TABLE ai_requests (
    id             uuid PRIMARY KEY,
    identity_id    text NOT NULL REFERENCES identities(id),
    session_id     uuid REFERENCES sessions(id),
    capability     text NOT NULL DEFAULT 'structured-generation',
    provider       text NOT NULL,
    model          text NOT NULL,
    prompt_name    text NOT NULL,
    prompt_version text NOT NULL,
    latency_ms     int NOT NULL,
    input_tokens   int NOT NULL DEFAULT 0,
    output_tokens  int NOT NULL DEFAULT 0,
    cost_usd       numeric(10,6) NOT NULL DEFAULT 0,
    success        boolean NOT NULL,
    error          text NOT NULL DEFAULT '',
    created_at     timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX ai_requests_identity_idx ON ai_requests (identity_id, created_at DESC);

-- +goose Down
DROP TABLE ai_requests;
