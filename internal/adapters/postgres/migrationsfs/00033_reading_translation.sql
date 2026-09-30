-- +goose Up
-- A non-Japanese article is stored as submitted (original_language 'und'),
-- translated by the worker, and then studied as Japanese: title and
-- paragraphs become the translation and the source moves here. NULL
-- original_paragraphs means "not translated yet", which is why 'ja' rows
-- never have any.
ALTER TABLE reading_articles
    ADD COLUMN original_language   text NOT NULL DEFAULT 'ja',
    ADD COLUMN original_title      text NOT NULL DEFAULT '',
    ADD COLUMN original_paragraphs jsonb;

-- +goose Down
ALTER TABLE reading_articles
    DROP COLUMN original_paragraphs,
    DROP COLUMN original_title,
    DROP COLUMN original_language;
