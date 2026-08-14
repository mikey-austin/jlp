-- +goose Up
CREATE TABLE documents (
    id          uuid PRIMARY KEY,
    session_id  uuid NOT NULL REFERENCES sessions(id),
    identity_id text NOT NULL REFERENCES identities(id),
    content     text NOT NULL DEFAULT '',
    version     int  NOT NULL DEFAULT 1,
    updated_at  timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX documents_session_idx ON documents (session_id);

CREATE TABLE document_versions (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id uuid NOT NULL REFERENCES documents(id),
    version     int NOT NULL,
    content     text NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE document_versions;
DROP TABLE documents;
