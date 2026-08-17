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
	"slices"
	"sync"
	"time"

	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// Every overridable settings key — see the app_settings migration's own
// table comment. Exported so a caller (this package's own tests, or a
// future admin tool) never has to hand-spell one of these strings a
// second time. Adding a provider means adding its key here, a keysFor
// case, a configModel case, and a providers entry; miss one and the
// /settings row silently resolves to the wrong thing, which is why
// service_test.go walks providers rather than naming them.
const (
	KeyOllamaModel     = "ai.ollama.model"
	KeyAnthropicModel  = "ai.anthropic.model"
	KeyGeminiModel     = "ai.gemini.model"
	KeyClaudeCLIModel  = "ai.claudecli.model"
	KeyClaudeCLIEffort = "ai.claudecli.effort"
	KeyCodexCLIModel   = "ai.codexcli.model"
	KeyCodexCLIEffort  = "ai.codexcli.effort"
	KeyAgyCLIModel     = "ai.agycli.model"
	KeyAgyCLIEffort    = "ai.agycli.effort"
)

// ErrUnknownProvider is returned by every mutating method when provider
// isn't one of the names in the providers catalog below.
var ErrUnknownProvider = errors.New("settings: unknown provider")

// ErrNoEffort is returned by SetEffort for a provider with no effort
// concept — the three API providers (ollama, anthropic, gemini) (see ports/ai.ModelResolver's
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
	{"gemini", "Gemini", false},
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
	case "gemini":
		return KeyGeminiModel, ""
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

	// ModelOptions is the enumerated model list for this provider
	// (Phase 4 Task M), genuinely observed from the provider itself —
	// never a fabricated guess — and always including Model even if the
	// provider no longer reports it (a removed model, or a value typed
	// by hand): see populateModelList's own doc comment. Empty means "no
	// working enumeration for this provider right now" — the /settings
	// template falls back to free text only, with ModelNote explaining
	// why.
	ModelOptions []string
	// ModelNote explains why ModelOptions is empty: either a provider
	// that structurally cannot enumerate (claudecli/codexcli — see the
	// task brief), or one that tried just now and failed (e.g. Ollama
	// unreachable). Always shown next to the free-text fallback; empty
	// when enumeration succeeded.
	ModelNote string
	// ModelAliases is claudecli's documented `--model` aliases
	// (sonnet/opus/haiku) offered as a clearly-labelled convenience —
	// see the task brief's "you MAY offer aliases... clearly labelled as
	// aliases, not an enumeration" note. Never populated for any other
	// provider.
	ModelAliases []string

	Effort        string
	EffortSource  string // "config" or "override" — only meaningful when HasEffort
	EffortOptions []string

	// Available reports whether THIS PROCESS actually constructed a
	// generator for this provider (Rows' `constructed` argument — the
	// same list cmd/jlp's buildAIGenerator returns and the workspace's
	// adapter picker uses). False means every control on this row is a
	// no-op: the override would be written, the badge would flip to
	// "override", and no request could ever reach the provider, because
	// buildAIGenerator's construction gates (an Anthropic API key, an
	// Ollama model) do not consult this Service at all.
	//
	// Whole-branch review I-2. The failure it prevents is specific and
	// nasty: on a host running Ollama with APP_AI_PROVIDER=fake and no
	// APP_AI_OLLAMA_MODEL, /settings showed a fully populated,
	// LIVE-ENUMERATED Ollama model dropdown — the ModelLister is built
	// unconditionally, independent of the generator — so the operator
	// picked a model, got a 303 and a success-shaped page, and nothing
	// changed anywhere. A success UI over a silent no-op is worse than
	// an error, and worse than an absent row.
	//
	// The row is kept rather than dropped so the operator learns WHY
	// the provider isn't offered (the /settings template renders the
	// reason and no controls); the write handlers reject the provider
	// too, so a hand-made POST can't sneak past the missing form.
	Available bool
}

// Service implements ports/ai.ModelResolver (see Model below) and
// drives the /settings page's reads and writes.
type Service struct {
	repo storage.SettingsRepository
	cfg  config.AI // immutable snapshot taken at construction — the fallback, never written back to

	mu        sync.RWMutex
	overrides map[string]string // key -> value; ABSENT key means "no override"

	// listers/modelCache back Rows' per-provider model enumeration
	// (Phase 4 Task M) — deliberately a SEPARATE lock from mu above:
	// enumerating models can hit the network or spawn a subprocess, and
	// must never hold mu while doing so, or a concurrent Model()/
	// SetModel() call (the hot path every AI request goes through) would
	// wait on Ollama or agy. Both maps are populated once at
	// construction and never have keys added or removed afterward, so
	// reading them needs no lock of its own — only each modelCacheEntry's
	// own mutex guards its mutable (models, fetchedAt) pair.
	listers    map[string]ai.ModelLister
	modelCache map[string]*modelCacheEntry
}

// modelCacheEntry holds one provider's cached enumeration result.
type modelCacheEntry struct {
	mu        sync.Mutex
	models    []string
	fetchedAt time.Time // zero means "never successfully fetched"
}

// modelListTimeout bounds a single ListModels attempt — a few seconds,
// per the task brief, so a slow or unreachable Ollama (or the agycli
// subprocess) never makes /settings hang.
const modelListTimeout = 3 * time.Second

// modelListTTL caches a SUCCESSFUL enumeration for this long before the
// next /settings render re-fetches — models change on the order of
// weeks (task brief), and `agy models` costs a real subprocess, so
// re-listing on every page load would be wasteful. A FAILED attempt is
// never cached (see listModels below): a later render always retries,
// so an operator who fixes Ollama sees it recover on their very next
// /settings visit rather than waiting out a stale failure.
const modelListTTL = time.Hour

// claudeCLIAliases are Claude Code's documented `--model` aliases
// (`claude --help`) — NOT an enumeration (the CLI has no models
// subcommand; see noListNoteClaudeCLI below), offered as a
// clearly-labelled convenience only.
var claudeCLIAliases = []string{"sonnet", "opus", "haiku"}

const (
	noListNoteClaudeCLI = "the Claude Code CLI has no models command — `claude models` is answered by the model itself, not enumerated. Type a model name, or pick a documented alias."
	noListNoteCodexCLI  = "the Codex CLI's model list isn't available non-interactively (`codex models` fails outside a terminal). Type a model name."
)

// NewService loads every current override from repo into an in-memory
// snapshot and returns a Service ready to resolve/serve immediately —
// see the package doc comment for why this one-time load (DB ->
// memory) is not the same thing as "copying config into the database"
// (that direction never happens).
//
// listers supplies a ports/ai.ModelLister for every provider that
// genuinely has one (today: "ollama", "anthropic", "agycli" — never
// "claudecli"/"codexcli", which cannot enumerate at all, see the task
// brief). A nil map, or a missing/nil entry for any provider, is valid
// and just means that provider's /settings row falls back to free
// text — enumeration is a lazily-invoked, best-effort capability, never
// required for this constructor to succeed (matching the "lazy, never
// at boot" rule: no lister is called here).
func NewService(ctx context.Context, repo storage.SettingsRepository, cfg config.AI, listers map[string]ai.ModelLister) (*Service, error) {
	cache := make(map[string]*modelCacheEntry, len(listers))
	for provider, lister := range listers {
		if lister == nil {
			continue
		}
		cache[provider] = &modelCacheEntry{}
	}

	s := &Service{repo: repo, cfg: cfg, overrides: map[string]string{}, listers: listers, modelCache: cache}
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
	case "gemini":
		return s.cfg.Gemini.Model
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
// for the /settings page — including each provider's model enumeration
// (Phase 4 Task M), attempted fresh (subject to caching — see
// listModels) on every call, bounded by ctx and modelListTimeout so a
// slow or absent provider never makes this call hang.
//
// constructed is the set of provider names this process actually built
// a generator for (cmd/jlp's buildAIGenerator return value, threaded
// through httpx.Options.AIProviders). It sets Row.Available — see that
// field for why a row the process cannot serve must not be rendered as
// a working control. Passed per call rather than held on the Service
// because the Service is itself the ai.ModelResolver every adapter is
// constructed WITH, so it necessarily exists before the answer does.
// An empty/nil constructed marks every row unavailable, which is the
// safe direction: it under-promises rather than over-promising.
//
// Enumeration is still attempted for unavailable providers and their
// values still reported: the page's job is to explain the deployment,
// and "Ollama is reachable and has these models, but this process built
// no Ollama generator" is a more useful diagnosis than silence.
func (s *Service) Rows(ctx context.Context, constructed []string) []Row {
	available := make(map[string]bool, len(constructed))
	for _, name := range constructed {
		available[name] = true
	}
	rows := s.baseRows()
	for i := range rows {
		rows[i].Available = available[rows[i].Provider]
		s.populateModelList(ctx, &rows[i])
	}
	return rows
}

// IsAvailable reports whether provider is one this process constructed
// a generator for — the same question Rows answers per row, exposed for
// the write handlers so a POST for an unavailable provider is rejected
// rather than written and reported as a success (whole-branch review
// I-2). Kept here beside Rows so the read and write paths cannot
// disagree about what "available" means.
func (s *Service) IsAvailable(provider string, constructed []string) bool {
	if modelKey, _ := keysFor(provider); modelKey == "" {
		return false // not a provider this Service knows at all
	}
	for _, name := range constructed {
		if name == provider {
			return true
		}
	}
	return false
}

// baseRows builds every Row's model/effort override-or-config value —
// the part that only ever reads s.overrides/s.cfg, held under mu for
// exactly as long as that takes and no longer, so the network/subprocess
// work populateModelList does afterward never happens while mu is held.
func (s *Service) baseRows() []Row {
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

// populateModelList fills row's ModelOptions/ModelNote/ModelAliases
// (Phase 4 Task M). claudecli/codexcli can never enumerate (see the
// task brief) and are handled with a fixed explanatory note — claudecli
// additionally offers its documented aliases, clearly labelled as
// aliases rather than an enumeration. Every other provider consults its
// ports/ai.ModelLister (when one was wired in — see NewService): on
// success, row.Model is folded into the result if the provider didn't
// report it itself, so an operator's already-set value is NEVER
// silently dropped from the dropdown; on failure, ModelOptions stays
// empty and ModelNote carries the reason, so /settings degrades to free
// text instead of fabricating or hiding the problem.
func (s *Service) populateModelList(ctx context.Context, row *Row) {
	switch row.Provider {
	case "claudecli":
		row.ModelAliases = slices.Clone(claudeCLIAliases)
		row.ModelNote = noListNoteClaudeCLI
		return
	case "codexcli":
		row.ModelNote = noListNoteCodexCLI
		return
	}

	lister := s.listers[row.Provider]
	if lister == nil {
		return // no capability wired for this provider — free text only, no note
	}

	models, err := s.listModels(ctx, row.Provider, lister)
	if err != nil {
		row.ModelNote = fmt.Sprintf("model list unavailable: %v", err)
		return
	}
	if row.Model != "" && !slices.Contains(models, row.Model) {
		models = append(slices.Clone(models), row.Model)
	}
	row.ModelOptions = models
}

// listModels returns provider's cached model list if it was fetched
// within modelListTTL, else attempts one fresh call to lister.ListModels
// bounded by modelListTimeout. A FAILED attempt is never written to the
// cache — an attempt ctx itself cancelled is not an answer about
// whether the provider actually has models, and a later call (the next
// /settings render) must always retry rather than remembering "no
// models" forever (see modelListTTL's own doc comment).
func (s *Service) listModels(ctx context.Context, provider string, lister ai.ModelLister) ([]string, error) {
	entry := s.modelCache[provider]
	if entry == nil {
		// Defensive only: NewService populates modelCache for every
		// provider with a non-nil lister, so a nil entry here should be
		// unreachable — but degrading to an uncached, unshared fetch
		// beats a nil-pointer panic.
		entry = &modelCacheEntry{}
	}

	entry.mu.Lock()
	defer entry.mu.Unlock()

	if !entry.fetchedAt.IsZero() && time.Since(entry.fetchedAt) < modelListTTL {
		return entry.models, nil
	}

	lctx, cancel := context.WithTimeout(ctx, modelListTimeout)
	defer cancel()
	models, err := lister.ListModels(lctx)
	if err != nil {
		return nil, err
	}
	entry.models, entry.fetchedAt = models, time.Now()
	return models, nil
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
