-- +goose Up
-- A draft is an article still being collected: the extension captures it
-- page by page, each page becomes ordered blocks (paragraphs and images)
-- that the learner can exclude before sending the whole as one article.
-- A learner has at most one. Blocks cascade with their draft, which is
-- how both discard and the 7-day sweep clean up. Image bytes live here,
-- like article figures, so the existing backup covers them.
CREATE TABLE reading_drafts (
    id           uuid PRIMARY KEY,
    identity_id  text NOT NULL REFERENCES identities(id),
    title        text NOT NULL DEFAULT '',
    source_name  text NOT NULL DEFAULT '',
    source_url   text NOT NULL DEFAULT '',
    author       text NOT NULL DEFAULT '',
    published_at timestamptz,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL,
    UNIQUE (identity_id)
);
-- seq is page*10000 + index within the page, so a re-captured page
-- keeps its slot in reading order without renumbering the others.
CREATE TABLE reading_draft_blocks (
    draft_id   uuid NOT NULL REFERENCES reading_drafts(id) ON DELETE CASCADE,
    seq        int  NOT NULL,
    page       int  NOT NULL,
    page_url   text NOT NULL DEFAULT '',
    kind       text NOT NULL,
    text       text NOT NULL DEFAULT '',
    caption    text NOT NULL DEFAULT '',
    alt        text NOT NULL DEFAULT '',
    media_type text NOT NULL DEFAULT '',
    width      int  NOT NULL DEFAULT 0,
    height     int  NOT NULL DEFAULT 0,
    sha256     text NOT NULL DEFAULT '',
    data       bytea,
    excluded   boolean NOT NULL DEFAULT false,
    PRIMARY KEY (draft_id, seq)
);
CREATE INDEX reading_drafts_updated_idx ON reading_drafts (updated_at);
-- +goose Down
DROP TABLE reading_draft_blocks;
DROP TABLE reading_drafts;
