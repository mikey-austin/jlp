-- +goose Up
ALTER TABLE corrections
    ADD COLUMN hint_ja     text NOT NULL DEFAULT '',
    ADD COLUMN hint_en     text NOT NULL DEFAULT '',
    ADD COLUMN attempts    int NOT NULL DEFAULT 0,
    ADD COLUMN confidence  int,
    ADD COLUMN revealed    boolean NOT NULL DEFAULT false,
    ADD CONSTRAINT corrections_confidence_range CHECK (confidence IS NULL OR confidence BETWEEN 1 AND 5);

-- +goose Down
ALTER TABLE corrections
    DROP CONSTRAINT corrections_confidence_range,
    DROP COLUMN hint_ja,
    DROP COLUMN hint_en,
    DROP COLUMN attempts,
    DROP COLUMN confidence,
    DROP COLUMN revealed;
