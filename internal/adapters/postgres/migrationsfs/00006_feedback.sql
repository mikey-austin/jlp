-- +goose Up
CREATE TABLE feedback_requests (
    id              uuid PRIMARY KEY,
    identity_id     text NOT NULL REFERENCES identities(id),
    session_id      uuid NOT NULL REFERENCES sessions(id),
    document_id     uuid NOT NULL REFERENCES documents(id),
    selection_start int NOT NULL,
    selection_end   int NOT NULL,
    selection_text  text NOT NULL,
    corrected_text  text NOT NULL DEFAULT '',
    ai_request_id   uuid,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX feedback_identity_idx ON feedback_requests (identity_id, created_at DESC);

CREATE TABLE corrections (
    id                  uuid PRIMARY KEY,
    feedback_request_id uuid NOT NULL REFERENCES feedback_requests(id),
    position            int NOT NULL,
    original            text NOT NULL,
    replacement         text NOT NULL,
    type                text NOT NULL,
    severity            text NOT NULL,
    explanation_ja      text NOT NULL DEFAULT '',
    explanation_en      text NOT NULL DEFAULT '',
    status              text NOT NULL DEFAULT 'presented',
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX corrections_feedback_idx ON corrections (feedback_request_id, position);

-- +goose Down
DROP TABLE corrections;
DROP TABLE feedback_requests;
