-- +goose Up
ALTER TABLE ai_requests ADD COLUMN agent text NOT NULL DEFAULT '';

-- +goose Down
ALTER TABLE ai_requests DROP COLUMN agent;
