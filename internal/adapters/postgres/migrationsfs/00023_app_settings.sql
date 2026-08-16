-- +goose Up
-- app_settings holds OPERATOR settings — today, which AI model/effort
-- each provider (ollama/anthropic/claudecli/codexcli) uses (Phase 4
-- Task S: "change the AI model at runtime from a settings page, no
-- restart"). This is deliberately GLOBAL, not identity-scoped: these
-- are deployment-wide knobs an operator changes from /settings, the
-- same audience as the APP_AI_* env vars they override — a future
-- reader must NOT "fix" this into being per-identity, the way most
-- other tables in this schema are. An absent row for a key means "no
-- override, use APP_AI_* config" (see
-- internal/ports/storage.SettingsRepository and
-- internal/application/settings.Service).
CREATE TABLE app_settings (
    key        text PRIMARY KEY,
    value      text NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now()
);

COMMENT ON TABLE app_settings IS
    'Operator settings (e.g. AI model/effort overrides) — GLOBAL, NOT identity-scoped. Do not add an identity_id column here; see internal/ports/storage.SettingsRepository.';

-- +goose Down
DROP TABLE app_settings;
