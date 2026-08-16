-- +goose Up
CREATE TABLE conversations (
    id          uuid PRIMARY KEY,
    session_id  uuid NOT NULL REFERENCES sessions(id),
    identity_id text NOT NULL REFERENCES identities(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX conversations_session_idx ON conversations (session_id);

CREATE TABLE conversation_turns (
    id              uuid PRIMARY KEY,
    conversation_id uuid NOT NULL REFERENCES conversations(id),
    position        int NOT NULL,
    learner_text    text NOT NULL,
    reply           text NOT NULL,
    reply_en        text NOT NULL DEFAULT '',
    followup        text NOT NULL DEFAULT '',
    corrections     jsonb NOT NULL DEFAULT '[]',
    ai_request_id   uuid,
    created_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX conversation_turns_conversation_idx ON conversation_turns (conversation_id, position);

-- +goose Down
DROP TABLE conversation_turns;
DROP TABLE conversations;
