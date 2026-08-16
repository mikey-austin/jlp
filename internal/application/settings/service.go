// Package settings is the application-layer home of Phase 4 Task S:
// letting an operator change which model (and, for the two CLI
// providers, effort level) each AI provider uses from a /settings
// page, taking effect on the very next AI request — no restart.
//
// Service is the seam that makes that possible. Every AI adapter
// (adapters/ollama, adapters/anthropic, adapters/clicmd's Claude and
// Codex generators) is constructed once at boot holding an
// ports/ai.ModelResolver instead of a fixed model string, and calls
// Model again on every single AI call rather than reading a value
// captured at construction. Service IS that ModelResolver: it keeps
// an in-memory, mutex-guarded snapshot of every current override,
// loaded from storage.SettingsRepository once at construction and
// kept in sync on every write this Service makes afterward — so a
// resolve never blocks on the database, no matter how many HTTP
// goroutines call it concurrently, while a settings save from a
// different goroutine safely updates the same snapshot.
//
// Config (cfg, an immutable snapshot of config.AI taken at
// construction and never written back to) is always the SEED and the
// FALLBACK: an absent override row means "use what APP_AI_* said".
// Service never copies cfg's values INTO the database — doing so at
// boot would silently freeze them as permanent overrides indistinct
// from an operator's own choice, defeating the whole "config stays the
// fallback" contract the task brief calls for.
package settings

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// The six overridable settings keys — see the app_settings migration's
// own table comment. Exported so a caller (this package's own tests,
// or a future admin tool) never has to hand-spell one of these
// strings a second time.
const (
	KeyOllamaModel     = "ai.ollama.model"
	KeyAnthropicModel  = "ai.anthropic.model"
	KeyClaudeCLIModel  = "ai.claudecli.model"
	KeyClaudeCLIEffort = "ai.claudecli.effort"
	KeyCodexCLIModel   = "ai.codexcli.model"
	KeyCodexCLIEffort  = "ai.codexcli.effort"
	KeyAgyCLIModel     = "ai.agycli.model"
	KeyAgyCLIEffort    = "ai.agycli.effort"
)

// ErrUnknownProvider is returned by every mutating method when
// provider isn't one of "ollama", "anthropic", "claudecli", "codexcli".
var ErrUnknownProvider = errors.New("settings: unknown provider")

// ErrNoEffort is returned by SetEffort when provider is "ollama" or
// "anthropic" — neither has an effort concept (see ports/ai.ModelResolver's
// doc comment), so the /settings page never renders that control for
// them, but this guards the same contract if it's ever reached anyway
// (e.g. a stale form, or a future non-UI caller).
var ErrNoEffort = errors.New("settings: this provider has no effort setting")

// ErrEmptyModel is returned by SetModel for a blank value — clearing a
// model override is Reset's job, not SetModel with "".
var ErrEmptyModel = errors.New("settings: model must not be empty (use Reset to clear an override)")

// providerInfo is the fixed catalog of overridable providers — order
// matters here, since Rows() below returns them in this order (the
// same order the /settings page renders them in, top to bottom).
type providerInfo struct {
	name      string
	label     string
	hasEffort bool
}

var providers = []providerInfo{
	{"ollama", "Ollama", false},
	{"anthropic", "Anthropic", false},
	{"claudecli", "Claude Code CLI", true},
	{"codexcli", "Codex CLI", true},
	{"agycli", "Antigravity CLI", true},
}

// keysFor returns provider's model/effort override keys. effortKey is
// "" for a provider with no effort concept. Both are "" for an
// unrecognized provider name.
func keysFor(provider string) (modelKey, effortKey string) {
	switch provider {
	case "ollama":
		return KeyOllamaModel, ""
	case "anthropic":
		return KeyAnthropicModel, ""
	case "claudecli":
		return KeyClaudeCLIModel, KeyClaudeCLIEffort
	case "codexcli":
		return KeyCodexCLIModel, KeyCodexCLIEffort
	case "agycli":
		return KeyAgyCLIModel, KeyAgyCLIEffort
	default:
		return "", ""
	}
}

// Row is one provider's line on the /settings page: its current
// EFFECTIVE value (override if one exists, else the config value) and
// where that value came from, for both model and (when HasEffort)
// effort.
type Row struct {
	Provider  string
	Label     string
	HasEffort bool

	Model       string
	ModelSource string // "config" or "override"

	Effort        string
	EffortSource  string // "config" or "override" — only meaningful when HasEffort
	EffortOptions []string
}

// Service implements ports/ai.ModelResolver (see Model below) and
// drives the /settings page's reads and writes.
type Service struct {
	repo storage.SettingsRepository
	cfg  config.AI // immutable snapshot taken at construction — the fallback, never written back to

	mu        sync.RWMutex
	overrides map[string]string // key -> value; ABSENT key means "no override"
}

// NewService loads every current override from repo into an in-memory
// snapshot and returns a Service ready to resolve/serve immediately —
// see the package doc comment for why this one-time load (DB ->
// memory) is not the same thing as "copying config into the database"
// (that direction never happens).
func NewService(ctx context.Context, repo storage.SettingsRepository, cfg config.AI) (*Service, error) {
	s := &Service{repo: repo, cfg: cfg, overrides: map[string]string{}}
	if err := s.reload(ctx); err != nil {
		return nil, fmt.Errorf("settings: load overrides: %w", err)
	}
	return s, nil
}

func (s *Service) reload(ctx context.Context) error {
	rows, err := s.repo.List(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]string, len(rows))
	for _, r := range rows {
		m[r.Key] = r.Value
	}
	s.mu.Lock()
	s.overrides = m
	s.mu.Unlock()
	return nil
}

// configModel/configEffort read the immutable cfg snapshot — the
// fallback whenever no override row exists for provider.
func (s *Service) configModel(provider string) string {
	switch provider {
	case "ollama":
		return s.cfg.Ollama.Model
	case "anthropic":
		return s.cfg.Anthropic.Model
	case "claudecli":
		return s.cfg.ClaudeCLI.Model
	case "codexcli":
		return s.cfg.CodexCLI.Model
	case "agycli":
		return s.cfg.AgyCLI.Model
	default:
		return ""
	}
}

func (s *Service) configEffort(provider string) string {
	switch provider {
	case "claudecli":
		return s.cfg.ClaudeCLI.Effort
	case "codexcli":
		return s.cfg.CodexCLI.Effort
	case "agycli":
		return s.cfg.AgyCLI.Effort
	default:
		return ""
	}
}

// Model implements ports/ai.ModelResolver: called on every AI request,
// potentially from many goroutines concurrently (see Service's own doc
// comment on the concurrency contract this RLock provides). An
// override always wins when present; an absent override falls back to
// cfg, exactly as if this method didn't exist at all.
func (s *Service) Model(provider string) (model, effort string) {
	modelKey, effortKey := keysFor(provider)

	s.mu.RLock()
	defer s.mu.RUnlock()

	if v, ok := s.overrides[modelKey]; ok {
		model = v
	} else {
		model = s.configModel(provider)
	}
	if effortKey != "" {
		if v, ok := s.overrides[effortKey]; ok {
			effort = v
		} else {
			effort = s.configEffort(provider)
		}
	}
	return model, effort
}

// Rows returns one Row per known provider, in a fixed display order,
// for the /settings page.
func (s *Service) Rows() []Row {
	s.mu.RLock()
	defer s.mu.RUnlock()

	rows := make([]Row, 0, len(providers))
	for _, p := range providers {
		modelKey, effortKey := keysFor(p.name)
		row := Row{Provider: p.name, Label: p.label, HasEffort: p.hasEffort}

		if v, ok := s.overrides[modelKey]; ok {
			row.Model, row.ModelSource = v, "override"
		} else {
			row.Model, row.ModelSource = s.configModel(p.name), "config"
		}

		if p.hasEffort {
			switch p.name {
			case "claudecli":
				row.EffortOptions = config.ClaudeEfforts()
			case "codexcli":
				row.EffortOptions = config.CodexEfforts()
			case "agycli":
				row.EffortOptions = config.AgyEfforts()
			}
			if v, ok := s.overrides[effortKey]; ok {
				row.Effort, row.EffortSource = v, "override"
			} else {
				row.Effort, row.EffortSource = s.configEffort(p.name), "config"
			}
		}

		rows = append(rows, row)
	}
	return rows
}

// SetModel saves a model override for provider, taking effect on the
// next AI request that reaches it (Model above). model must be
// non-empty — clearing an override is Reset's job.
func (s *Service) SetModel(ctx context.Context, provider, model string) error {
	modelKey, _ := keysFor(provider)
	if modelKey == "" {
		return fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	if model == "" {
		return ErrEmptyModel
	}
	return s.setOverride(ctx, modelKey, model)
}

// SetEffort saves an effort override for provider, validated against
// that TOOL's own accepted vocabulary via config.ValidateEffort — the
// one shared definition config.Load's own boot-time validation uses,
// so a value only Claude Code accepts (e.g. "max") is rejected for
// Codex and vice versa (e.g. "minimal"). An invalid value is rejected
// here, before s.repo.Set is ever called — it is never written to the
// database.
func (s *Service) SetEffort(ctx context.Context, provider, effort string) error {
	modelKey, effortKey := keysFor(provider)
	if modelKey == "" {
		return fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	if effortKey == "" {
		return fmt.Errorf("%w: %q", ErrNoEffort, provider)
	}
	if effort == "" {
		return s.deleteOverride(ctx, effortKey)
	}
	if err := config.ValidateEffort(provider, effort); err != nil {
		return fmt.Errorf("settings: %w", err)
	}
	return s.setOverride(ctx, effortKey, effort)
}

// Reset deletes every override row for provider (model, and effort
// when the provider has one) — "reset to config".
func (s *Service) Reset(ctx context.Context, provider string) error {
	modelKey, effortKey := keysFor(provider)
	if modelKey == "" {
		return fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
	if err := s.deleteOverride(ctx, modelKey); err != nil {
		return err
	}
	if effortKey != "" {
		if err := s.deleteOverride(ctx, effortKey); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) setOverride(ctx context.Context, key, value string) error {
	if err := s.repo.Set(ctx, key, value); err != nil {
		return err
	}
	s.mu.Lock()
	s.overrides[key] = value
	s.mu.Unlock()
	return nil
}

func (s *Service) deleteOverride(ctx context.Context, key string) error {
	if err := s.repo.Delete(ctx, key); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.overrides, key)
	s.mu.Unlock()
	return nil
}
