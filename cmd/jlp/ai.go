package main

import (
	"fmt"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/adapters/agycli"
	"github.com/mikeyaustin/jlp/internal/adapters/airouter"
	"github.com/mikeyaustin/jlp/internal/adapters/anthropic"
	"github.com/mikeyaustin/jlp/internal/adapters/clicmd"
	"github.com/mikeyaustin/jlp/internal/adapters/fakeai"
	"github.com/mikeyaustin/jlp/internal/adapters/ollama"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/observability"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
	"github.com/mikeyaustin/jlp/internal/ports/storage"
)

// aiPricing is the USD-per-million-token rate card the observability
// decorator costs every AI call against. It lives here rather than in
// package config because it's not deployment configuration an operator
// tunes per environment — it's a fixed fact about what providers
// charge, reviewed and updated in code alongside the provider list
// itself.
//
// Ollama models run locally with no per-token billing, so they're
// listed explicitly at $0/$0 rather than simply left out: an absent
// entry also costs $0 (observability.NewAIObserver treats a pricing
// miss as free — see its own doc comment), but leaving it out would
// trip buildAIGenerator's "model has no pricing entry" warning below
// for every routine local-model call. qwen3:4b is the model `make
// ollama-pull` documents pulling by default (see the Makefile and
// README's AI providers section); any OTHER local model an operator
// points APP_AI_OLLAMA_MODEL at still works, it just logs that warning
// once at boot as a heads-up that its cost won't be tracked (it's
// already $0, same as every unpriced model).
//
// "cli" is the fixed Model value BOTH clicmd adapters report
// (internal/adapters/clicmd — Task 12): claudecli and codexcli always
// report zero tokens (see the package doc comment: neither CLI's
// non-interactive JSON output exposes a token count), so cost is $0
// regardless of what's in this table. It's listed here anyway, same
// reasoning as qwen3:4b, purely so warnIfUnpriced never has cause to
// fire for it — moot in practice since buildAIGenerator doesn't call
// warnIfUnpriced for claudecli/codexcli (they're unconditionally
// constructed, like fake, not gated on config the way
// anthropic/ollama are), but kept explicit rather than relying on
// that being the reason it's silent.
func aiPricing() map[string]observability.ModelPricing {
	return map[string]observability.ModelPricing{
		"claude-sonnet-5": {InPerMTok: 3, OutPerMTok: 15},
		"fake-1":          {InPerMTok: 0, OutPerMTok: 0},
		"qwen3:4b":        {InPerMTok: 0, OutPerMTok: 0},
		"cli":             {InPerMTok: 0, OutPerMTok: 0},
	}
}

// knownPromptNames is every prompt name a route (APP_AI_ROUTES) or the
// app itself can actually send to GenerateStructured — kept here as a
// small, manually maintained slice because nothing in the codebase
// enumerates prompt names centrally (each agent package defines its
// own promptName consts privately: see
// internal/agent/teacher/teacher.go's promptName and
// internal/agent/drill/drill.go's generatePromptName/
// evaluatePromptName). Add a new entry here whenever an agent gains a
// new prompt name; forgetting to is harmless on its own (see
// warnForUnknownPromptNames below — it only warns, never errors) but
// leaves a future operator's typo undetected.
var knownPromptNames = []string{
	"teacher.feedback",
	"drill.generate",
	"drill.evaluate",
	"summary.generate",
	// teacher.agentic is never sent to GenerateStructured (it's the
	// ONLY entry in knownToolPromptNames below, sent to CallWithTools
	// instead) — listed here too purely so warnForUnknownPromptNames
	// doesn't fire a spurious "unknown prompt name" warning for the
	// one legitimate tool-calling route
	// (APP_AI_ROUTES=teacher.agentic=...): that function scans every
	// route regardless of which capability it's actually for (see its
	// own doc comment on "an operator's typo"), and it has no way to
	// know this route is buildToolCaller's business, not
	// buildAIGenerator's.
	"teacher.agentic",
}

// knownToolPromptNames is every prompt name a route can name for TOOL
// CALLING specifically (buildToolCaller below) — a much narrower list
// than knownPromptNames above, which is buildAIGenerator's (structured
// generation only ever goes through a handful of Rule-4 schema-backed
// prompts; tool-calling conversations are rarer still, today just the
// agentic teacher's investigation step). buildToolCaller only
// validates/resolves APP_AI_ROUTES entries whose promptName appears
// here — see that function's own doc comment for why: APP_AI_ROUTES
// is one shared config value for BOTH buildAIGenerator and
// buildToolCaller, so an entry like "teacher.feedback=claudecli"
// (perfectly valid for the structured-generation path
// buildAIGenerator already resolved) must not also be interpreted as
// a tool-calling route just because it's present in the same string —
// claudecli doesn't implement ai.ToolCaller, and "teacher.feedback"
// was never a tool-calling prompt name to begin with.
var knownToolPromptNames = []string{
	"teacher.agentic",
}

// unknownPromptNames returns every key of routes absent from
// knownPromptNames. Split out from warnForUnknownPromptNames so a test
// can assert on exactly what would be warned about without swapping
// slog's default handler — see cmd/jlp/ai_test.go's
// TestUnknownPromptNamesFlagsNamesOutsideTheKnownList.
func unknownPromptNames(routes map[string][]string) []string {
	known := make(map[string]bool, len(knownPromptNames))
	for _, n := range knownPromptNames {
		known[n] = true
	}
	var unknown []string
	for promptName := range routes {
		if !known[promptName] {
			unknown = append(unknown, promptName)
		}
	}
	return unknown
}

// warnForUnknownPromptNames logs a boot-time slog.Warn for every
// APP_AI_ROUTES entry naming a prompt outside knownPromptNames — most
// likely an operator's typo (e.g. the README once documented
// "drill.exercise", which was never a real prompt name — see
// internal/agent/drill/drill.go), or knownPromptNames itself having
// gone stale after a new prompt was added elsewhere. This is a
// warning, not a boot error, symmetric with warnIfUnpriced above: an
// unmatched route promptName isn't a routing failure by itself — that
// PromptName simply never arrives via GenerateStructured, so the route
// silently never fires and every real call falls through to
// APP_AI_PROVIDER's default. The operator deserves to know at boot
// rather than discover a route quietly doing nothing.
func warnForUnknownPromptNames(routes map[string][]string) {
	for _, promptName := range unknownPromptNames(routes) {
		slog.Warn("ai: route names a prompt not in the known prompt list, this route will never match a real request", "prompt_name", promptName, "known_prompt_names", knownPromptNames)
	}
}

// buildAIGenerator turns cfg.AI (Provider, Anthropic, Ollama, ClaudeCLI,
// CodexCLI, Routes) into the single ai.StructuredGenerator the rest of
// main wires everywhere an AI call is made (teacher, drill): an
// airouter.New on top of a per-provider-name map of
// observability.NewAIObserver-wrapped adapters.
//
// Each provider is individually wrapped by the observer BEFORE it's
// handed to the router — never the other way around — so a failover
// from one provider to the next is recorded as two separate
// ai_requests rows, one per attempt, each correctly attributed to the
// provider that made it (PRD §23/§24's "failover pairs visible in
// ai_requests" requirement). Only a CONFIGURED provider is built at
// all: fake, claudecli, and codexcli always (fake because it's
// offline and free, so a fallback chain can never fail to construct;
// claudecli/codexcli because internal/adapters/clicmd's New* never
// errors at construction — a missing `claude`/`codex` binary is a
// per-call error, not a boot one, see that package's doc comment),
// anthropic once an API key is set, ollama once it's actually needed
// (named by APP_AI_PROVIDER or an APP_AI_ROUTES chain) AND has a model
// configured.
//
// A route or APP_AI_PROVIDER naming a provider that isn't
// constructible — in practice only ollama with no
// APP_AI_OLLAMA_MODEL set, now that Task 12 makes claudecli/codexcli
// unconditionally constructible too — is a boot error: the operator
// asked for a specific chain and deserves to find out at startup that
// a link in it is missing, not on the first request that happens to
// need it.
// resolver, when non-nil, is consulted by every constructed adapter on
// every call (ports/ai.ModelResolver — Phase 4 Task S) so a /settings
// override reaches the very next AI request without reconstructing
// anything buildAIGenerator built. nil is a legitimate value (`jlp
// eval`'s one-shot corpus run has no database connection to load
// overrides from, so it passes nil and always uses cfg verbatim,
// exactly like before this task) — every adapter treats a nil
// resolver as "no override capability", not an error.
//
// The second and third return values back Phase 4 Task W item 5's
// workspace per-request adapter-override dropdown: available is the
// fixed, real-provider display order (ollama, anthropic, claudecli,
// codexcli, agycli — deliberately excluding "fake", which is always
// constructible but never a meaningful operator choice — see
// aiProviderPriority's own doc comment) filtered to whichever of those
// are actually constructible in THIS deployment, and defaultProvider is
// the highest-priority one: the first provider in the "teacher.feedback"
// APP_AI_ROUTES chain when one exists, else APP_AI_PROVIDER — even when
// that's "fake" (every non-integration test's config, and `jlp eval`),
// in which case it's included in available too despite the exclusion
// above, so the dropdown's own preselected default is never an option
// missing from its own list.
func buildAIGenerator(cfg config.Config, aiRequestRepo storage.AIRequestRepository, resolver ai.ModelResolver) (ai.StructuredGenerator, []string, string, error) {
	routes, err := config.ParseRoutes(cfg.AI.Routes)
	if err != nil {
		return nil, nil, "", err
	}
	warnForUnknownPromptNames(routes)

	needed := map[string]bool{cfg.AI.Provider: true}
	for _, chain := range routes {
		for _, name := range chain {
			needed[name] = true
		}
	}

	pricing := aiPricing()
	raw := map[string]ai.StructuredGenerator{
		"fake": fakeai.New(),
		// claudecli/codexcli are unconditionally constructible, same as
		// fake — see the doc comment above. No warnIfUnpriced call for
		// either: they're not gated on config the way anthropic/ollama
		// are, so there's no "configured but maybe unused" case to warn
		// about, and their tokens are always 0 regardless (cost is $0
		// either way — see aiPricing's "cli" entry doc comment).
		"claudecli": clicmd.NewClaude(cfg.AI.ClaudeCLI, resolver),
		"codexcli":  clicmd.NewCodex(cfg.AI.CodexCLI, resolver),
		// agycli is the same kind of host-mode CLI adapter, constructible
		// unconditionally for the same reason. Unlike the other two it
		// reports real token counts (the CLI's envelope carries a usage
		// block), but its model names aren't in the pricing table, so
		// cost still records as $0.
		"agycli": agycli.New(cfg.AI.AgyCLI, resolver),
	}
	// anthropic and ollama are gated differently ON PURPOSE, per the
	// task brief: anthropic only needs one field (APIKey) to become
	// constructible, and constructing an unused client is free (no
	// network call happens until GenerateStructured is actually
	// called) — so "key set" alone is enough to build it, whether or
	// not anything currently routes to it. ollama's gate is stricter
	// (needed[...] AND a model configured) only because Model has no
	// default: building it unconditionally on a bare "ollama" mention
	// somewhere would require inventing a fake default model name, and
	// silently constructing a generator nothing can actually reach a
	// server for is worse than requiring the config that makes it real.
	// Neither gate is observable from outside buildAIGenerator's return
	// value: a provider that's built but never routed to is inert, so
	// this asymmetry only affects whether its startup pricing warning
	// (below) fires for a configured-but-unused provider.
	if cfg.AI.Anthropic.APIKey != "" {
		raw["anthropic"] = anthropic.New(cfg.AI.Anthropic, resolver)
		warnIfUnpriced(pricing, "anthropic", cfg.AI.Anthropic.Model)
	}
	if needed["ollama"] && cfg.AI.Ollama.Model != "" {
		raw["ollama"] = ollama.New(cfg.AI.Ollama, resolver)
		warnIfUnpriced(pricing, "ollama", cfg.AI.Ollama.Model)
	}

	providers := make(map[string]ai.StructuredGenerator, len(raw))
	for name, gen := range raw {
		providers[name] = observability.NewAIObserver(gen, aiRequestRepo, pricing)
	}

	resolvedRoutes := make(map[string][]ai.StructuredGenerator, len(routes))
	for promptName, chain := range routes {
		for _, name := range chain {
			gen, ok := providers[name]
			if !ok {
				return nil, nil, "", fmt.Errorf("ai: route %q names provider %q, which isn't constructible (missing config, e.g. APP_AI_OLLAMA_MODEL for ollama or APP_AI_ANTHROPIC_APIKEY for anthropic)", promptName, name)
			}
			resolvedRoutes[promptName] = append(resolvedRoutes[promptName], gen)
		}
	}

	defaultGen, ok := providers[cfg.AI.Provider]
	if !ok {
		return nil, nil, "", fmt.Errorf("ai: APP_AI_PROVIDER %q isn't constructible (missing config, e.g. APP_AI_OLLAMA_MODEL for ollama or APP_AI_ANTHROPIC_APIKEY for anthropic)", cfg.AI.Provider)
	}

	// defaultProvider is Phase 4 Task W item 5's "highest priority"
	// provider: the first link in the "teacher.feedback" route chain
	// when APP_AI_ROUTES configures one (resolvedRoutes above already
	// proved every link in it is constructible), else APP_AI_PROVIDER —
	// which defaultGen just proved is constructible too.
	defaultProvider := cfg.AI.Provider
	if chain, ok := routes["teacher.feedback"]; ok && len(chain) > 0 {
		defaultProvider = chain[0]
	}

	// available is aiProviderPriority filtered to providers actually
	// constructible here, with defaultProvider prepended when it isn't
	// already in that filtered list (only possible for "fake" — see
	// this function's own doc comment) so the dropdown's default is
	// never missing from its own option list.
	available := make([]string, 0, len(aiProviderPriority)+1)
	for _, name := range aiProviderPriority {
		if _, ok := providers[name]; ok {
			available = append(available, name)
		}
	}
	if _, ok := providers[defaultProvider]; ok {
		found := false
		for _, name := range available {
			if name == defaultProvider {
				found = true
				break
			}
		}
		if !found {
			available = append([]string{defaultProvider}, available...)
		}
	}

	return airouter.New(resolvedRoutes, []ai.StructuredGenerator{defaultGen}, providers), available, defaultProvider, nil
}

// aiProviderPriority is the fixed display order for the workspace's
// per-request adapter-override dropdown (Phase 4 Task W item 5) —
// deliberately the SAME provider set and order as
// application/settings.Service's own providers catalog (its persistent
// /settings overrides are a different concept from this per-request
// override, but there's no reason for an operator to see the four real
// providers listed in two different orders across the app). "fake" is
// excluded on purpose: fakeai.New() is unconditionally constructed
// above (offline, free, always available as a fallback-chain safety
// net), so it WOULD pass an "actually constructible" test, but it's
// never a choice a real operator wants to see next to Ollama/Anthropic/
// the CLIs — buildAIGenerator's caller (this function) still includes
// it when it's genuinely the configured default (every non-integration
// test, `jlp eval`), just not as an ordinary option alongside the real
// providers.
var aiProviderPriority = []string{"ollama", "anthropic", "claudecli", "codexcli", "agycli"}

// buildToolCaller turns cfg.AI into the single ai.ToolCaller Phase 4's
// agentic capabilities call (Task 2 onward) — the ToolCaller
// counterpart of buildAIGenerator above, an airouter.NewToolCaller on
// top of a per-provider-name map of observability.NewToolObserver-
// wrapped adapters. It shares buildAIGenerator's route parsing and
// pricing table, but its provider set is narrower: only
// adapters/fakeai, adapters/anthropic, and adapters/ollama implement
// ai.ToolCaller today — adapters/clicmd's claudecli/codexcli adapters
// (their non-interactive JSON CLI output has no tool-calling protocol
// of its own to drive) do not, so APP_AI_PROVIDER naming either for
// TOOL calling is a boot error here, the same "operator asked for a
// chain link that doesn't exist" treatment buildAIGenerator gives an
// unconstructible provider.
//
// Route validation is deliberately narrower than buildAIGenerator's,
// too: APP_AI_ROUTES is ONE shared config string covering both
// capabilities, so this function only validates/resolves entries whose
// promptName is in knownToolPromptNames — an entry like
// "teacher.feedback=claudecli" is a perfectly valid structured-
// generation route (buildAIGenerator already resolved it) that simply
// isn't relevant here, and must not fail boot just because claudecli
// doesn't implement ai.ToolCaller. Called unconditionally from
// main() (cheap — no network call happens at construction, same as
// buildAIGenerator): the agentic teacher is opt-in per request
// (APP_AI_AGENTICTEACHER), but the ToolCaller itself is built once at
// boot regardless, so turning the flag on later never needs a restart
// bug hunt.
func buildToolCaller(cfg config.Config, aiRequestRepo storage.AIRequestRepository, resolver ai.ModelResolver) (ai.ToolCaller, error) {
	routes, err := config.ParseRoutes(cfg.AI.Routes)
	if err != nil {
		return nil, err
	}

	pricing := aiPricing()
	raw := map[string]ai.ToolCaller{
		"fake": fakeai.New(),
	}
	if cfg.AI.Anthropic.APIKey != "" {
		raw["anthropic"] = anthropic.New(cfg.AI.Anthropic, resolver)
	}
	needed := map[string]bool{cfg.AI.Provider: true}
	for _, chain := range routes {
		for _, name := range chain {
			needed[name] = true
		}
	}
	if needed["ollama"] && cfg.AI.Ollama.Model != "" {
		raw["ollama"] = ollama.New(cfg.AI.Ollama, resolver)
	}

	providers := make(map[string]ai.ToolCaller, len(raw))
	for name, gen := range raw {
		providers[name] = observability.NewToolObserver(gen, aiRequestRepo, pricing)
	}

	toolPromptNames := make(map[string]bool, len(knownToolPromptNames))
	for _, name := range knownToolPromptNames {
		toolPromptNames[name] = true
	}

	resolvedRoutes := make(map[string][]ai.ToolCaller, len(routes))
	for promptName, chain := range routes {
		// Skip routes for prompt names this function doesn't own — see
		// the doc comment above. An unrelated route naming claudecli/
		// codexcli for a StructuredGenerator-only prompt (e.g.
		// "teacher.feedback=claudecli") is buildAIGenerator's concern,
		// already resolved there; it must not also fail HERE just
		// because claudecli isn't an ai.ToolCaller.
		if !toolPromptNames[promptName] {
			continue
		}
		for _, name := range chain {
			gen, ok := providers[name]
			if !ok {
				return nil, fmt.Errorf("ai: tool-calling route %q names provider %q, which doesn't implement ai.ToolCaller (only fake, anthropic, and ollama do)", promptName, name)
			}
			resolvedRoutes[promptName] = append(resolvedRoutes[promptName], gen)
		}
	}

	defaultGen, ok := providers[cfg.AI.Provider]
	if !ok {
		return nil, fmt.Errorf("ai: APP_AI_PROVIDER %q doesn't implement ai.ToolCaller (only fake, anthropic, and ollama do)", cfg.AI.Provider)
	}

	return airouter.NewToolCaller(resolvedRoutes, []ai.ToolCaller{defaultGen}), nil
}

// warnIfUnpriced logs a startup warning when provider's model has no
// aiPricing() entry — the call still succeeds either way (an absent
// entry already costs $0, see aiPricing's doc comment), but an
// operator running a live-billed model under an unrecognized name
// deserves to know at boot that its cost won't be tracked, rather than
// silently watching the /ai page report $0 forever.
func warnIfUnpriced(pricing map[string]observability.ModelPricing, provider, model string) {
	if model == "" {
		return
	}
	if _, ok := pricing[model]; !ok {
		slog.Warn("ai: model has no pricing entry, cost will be recorded as $0", "provider", provider, "model", model)
	}
}
