package storage

import "context"

// AppSetting is one row of the app_settings key/value table: an
// OPERATOR setting (which AI model/effort each provider uses, Phase 4
// Task S), never learner/identity data — see the migration's own
// table comment for why this is deliberately global, not
// identity-scoped, and application/settings.Service for the fixed set
// of keys ("ai.ollama.model", "ai.claudecli.effort", etc.) actually
// used.
type AppSetting struct {
	Key   string
	Value string
}

// SettingsRepository persists operator overrides for which AI
// model/effort each provider uses (Phase 4 Task S). Global, not
// identity-scoped — see AppSetting's doc comment. An absent row for a
// key means "no override, fall back to APP_AI_* config" —
// application/settings.Service, the only caller, is what implements
// that fallback; this port only ever reports what IS or ISN'T
// currently overridden.
//
// PRIVILEGE SCOPE — stated here because it is not obvious from the
// method set (whole-branch review I-1/F7). "Global" means global in
// BOTH directions: there is no identity column to scope a read by, and
// there is no role check above this port either
// (internal/adapters/http/settings.go). Any authenticated learner who
// can reach /settings changes the AI model and effort for EVERY learner
// in the deployment. That is acceptable under JLP's documented
// single-learner posture (PRD §2) and is the intended design — the
// migration's table comment is right that a future reader must not
// "fix" this into being per-identity — but it IS a cross-learner write
// surface, and a multi-identity deployment must be aware of it. See the
// deployment-constraint note in README's "Changing model/effort at
// runtime" section. Tightening it would need a role system AND a change
// to this port (List takes no identity), neither of which is in scope
// for Phase 4; documented rather than half-built.
type SettingsRepository interface {
	// Set upserts key=value — the "change this provider's model/effort"
	// action.
	Set(ctx context.Context, key, value string) error
	// Delete removes any override row for key — the "reset to config"
	// action. Deleting a key with no existing row is not an error.
	Delete(ctx context.Context, key string) error
	// List returns every current override row, in no particular
	// caller-relevant order. Used both to seed application/settings.
	// Service's in-memory snapshot at construction and to render the
	// /settings page.
	List(ctx context.Context) ([]AppSetting, error)
}
