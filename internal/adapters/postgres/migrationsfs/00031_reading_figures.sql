-- +goose Up
-- An article's images, captured by the extension from the learner's own
-- tab and uploaded with the text (the server never fetches a page).
-- Bytes live here rather than on disk so the existing restic backup of
-- this database covers them; an article carries at most 12, each at
-- most 2 MB after the client's downscale.
CREATE TABLE reading_article_figures (
    article_id      uuid NOT NULL REFERENCES reading_articles(id),
    ordinal         int  NOT NULL,
    after_paragraph int  NOT NULL,
    caption         text NOT NULL DEFAULT '',
    alt             text NOT NULL DEFAULT '',
    is_lead         boolean NOT NULL DEFAULT false,
    in_text         boolean NOT NULL DEFAULT true,
    media_type      text NOT NULL,
    width           int  NOT NULL,
    height          int  NOT NULL,
    sha256          text NOT NULL,
    data            bytea NOT NULL,
    PRIMARY KEY (article_id, ordinal)
);
-- +goose Down
DROP TABLE reading_article_figures;
