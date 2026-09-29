-- +goose Up
-- The 読解 pipeline: an article the learner chose to read, the AI study
-- editions built from it, and their Send-to-Kindle deliveries. Three
-- tables, not one, because the three have independent lifecycles: an
-- article is analysed again (a new prompt version) without being
-- re-ingested, and an edition is delivered again (a lost email) without
-- being re-analysed.
CREATE TABLE reading_articles (
    id           uuid PRIMARY KEY,
    identity_id  text NOT NULL REFERENCES identities(id),
    source_url   text NOT NULL DEFAULT '',
    source_name  text NOT NULL DEFAULT '',
    title        text NOT NULL,
    author       text NOT NULL DEFAULT '',
    published_at timestamptz,
    paragraphs   jsonb NOT NULL,
    content_hash text NOT NULL,
    created_at   timestamptz NOT NULL,
    deleted_at   timestamptz
);
-- Idempotent ingestion: one identity, one body, one article. A second
-- submission of the same text finds this row instead of paying for a
-- second analysis. Deleted rows keep their slot on purpose — submitting
-- a deleted article again restores it (see UpsertReadingArticle).
CREATE UNIQUE INDEX reading_articles_identity_hash_uq ON reading_articles (identity_id, content_hash);
CREATE INDEX reading_articles_identity_idx ON reading_articles (identity_id, created_at DESC);

CREATE TABLE reading_editions (
    id                 uuid PRIMARY KEY,
    article_id         uuid NOT NULL REFERENCES reading_articles(id),
    identity_id        text NOT NULL REFERENCES identities(id),
    status             text NOT NULL CHECK (status IN ('pending', 'analysing', 'ready', 'failed')),
    prompt_name        text NOT NULL,
    prompt_version     text NOT NULL,
    schema_name        text NOT NULL,
    lesson             jsonb,
    ai_request_id      text NOT NULL DEFAULT '',
    attempts           integer NOT NULL DEFAULT 0,
    last_error         text NOT NULL DEFAULT '',
    deliver_when_ready boolean NOT NULL DEFAULT false,
    next_attempt_at    timestamptz NOT NULL,
    claimed_at         timestamptz,
    created_at         timestamptz NOT NULL,
    updated_at         timestamptz NOT NULL
);
CREATE INDEX reading_editions_article_idx ON reading_editions (article_id, created_at DESC);
-- The worker's claim query scans only work that is waiting or stuck.
CREATE INDEX reading_editions_due_idx ON reading_editions (next_attempt_at)
    WHERE status IN ('pending', 'analysing');

CREATE TABLE reading_deliveries (
    id              uuid PRIMARY KEY,
    edition_id      uuid NOT NULL REFERENCES reading_editions(id),
    identity_id     text NOT NULL REFERENCES identities(id),
    destination     text NOT NULL,
    status          text NOT NULL CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    last_error      text NOT NULL DEFAULT '',
    next_attempt_at timestamptz NOT NULL,
    claimed_at      timestamptz,
    created_at      timestamptz NOT NULL,
    sent_at         timestamptz
);
-- Duplicate prevention: at most one IN-FLIGHT delivery of an edition to
-- a destination. A double click, or the extension's auto-send racing a
-- manual 送信, cannot mail the same book twice; a deliberate resend
-- after a delivery has finished (sent or failed) is still allowed.
CREATE UNIQUE INDEX reading_deliveries_inflight_uq ON reading_deliveries (edition_id, destination)
    WHERE status IN ('pending', 'sending');
CREATE INDEX reading_deliveries_edition_idx ON reading_deliveries (edition_id, created_at DESC);
CREATE INDEX reading_deliveries_due_idx ON reading_deliveries (next_attempt_at)
    WHERE status IN ('pending', 'sending');

-- +goose Down
DROP TABLE reading_deliveries;
DROP TABLE reading_editions;
DROP TABLE reading_articles;
