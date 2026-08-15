-- +goose Up
-- Phase 3 Task 8: POST /api/v1/words (Nihongo Daily's bulk vocabulary
-- sync contract) needs an English gloss alongside vocabulary_items'
-- existing Japanese `meaning`, and free-form tags a source deck may
-- carry ("education", "noun", ...). Both default to their empty value
-- so every existing row (and every row created by the pre-existing
-- vocabulary.lookup path, which never sets them) reads back as
-- ""/[] rather than NULL.
ALTER TABLE vocabulary_items ADD COLUMN meaning_en text NOT NULL DEFAULT '';
ALTER TABLE vocabulary_items ADD COLUMN tags jsonb NOT NULL DEFAULT '[]';

-- +goose Down
ALTER TABLE vocabulary_items DROP COLUMN tags;
ALTER TABLE vocabulary_items DROP COLUMN meaning_en;
