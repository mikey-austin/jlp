-- +goose Up
CREATE TABLE grammar_concepts (
    slug          text PRIMARY KEY,
    name          text NOT NULL,
    jlpt_level    int NOT NULL CHECK (jlpt_level BETWEEN 1 AND 5),
    description   text NOT NULL DEFAULT '',
    examples      jsonb NOT NULL DEFAULT '[]',
    related       jsonb NOT NULL DEFAULT '[]',
    prerequisites jsonb NOT NULL DEFAULT '[]'
);

CREATE TABLE correction_concepts (
    correction_id uuid NOT NULL REFERENCES corrections(id),
    concept_slug  text NOT NULL, -- NOT an FK: unknown slugs are recorded unresolved
    resolved      boolean NOT NULL DEFAULT true,
    PRIMARY KEY (correction_id, concept_slug)
);

-- +goose Down
DROP TABLE correction_concepts;
DROP TABLE grammar_concepts;
