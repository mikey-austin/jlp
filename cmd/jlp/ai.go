package main

import (
	"fmt"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/adapters/airouter"
	"github.com/mikeyaustin/jlp/internal/adapters/anthropic"
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
func aiPricing() map[string]observability.ModelPricing {
	return map[string]observability.ModelPricing{
		"claude-sonnet-5": {InPerMTok: 3, OutPerMTok: 15},
		"fake-1":          {InPerMTok: 0, OutPerMTok: 0},
		"qwen3:4b":        {InPerMTok: 0, OutPerMTok: 0},
	}
}

// buildAIGenerator turns cfg.AI (Provider, Anthropic, Ollama, Routes)
// into the single ai.StructuredGenerator the rest of main wires
// everywhere an AI call is made (teacher, drill): an airouter.New on
// top of a per-provider-name map of observability.NewAIObserver-
// wrapped adapters.
//
// Each provider is individually wrapped by the observer BEFORE it's
// handed to the router — never the other way around — so a failover
// from one provider to the next is recorded as two separate
// ai_requests rows, one per attempt, each correctly attributed to the
// provider that made it (PRD §23/§24's "failover pairs visible in
// ai_requests" requirement). Only a CONFIGURED provider is built at
// all: fake always (offline, free, so a fallback chain can never fail
// to construct), anthropic once an API key is set, ollama once it's
// actually needed (named by APP_AI_PROVIDER or an APP_AI_ROUTES
// chain) AND has a model configured.
//
// A route or APP_AI_PROVIDER naming a provider that isn't
// constructible — most commonly claudecli/codexcli (Task 12's CLI
// adapters, not implemented yet) or ollama with no
// APP_AI_OLLAMA_MODEL set — is a boot error: the operator asked for a
// specific chain and deserves to find out at startup that a link in
// it is missing, not on the first request that happens to need it.
func buildAIGenerator(cfg config.Config, aiRequestRepo storage.AIRequestRepository) (ai.StructuredGenerator, error) {
	routes, err := config.ParseRoutes(cfg.AI.Routes)
	if err != nil {
		return nil, err
	}

	needed := map[string]bool{cfg.AI.Provider: true}
	for _, chain := range routes {
		for _, name := range chain {
			needed[name] = true
		}
	}

	pricing := aiPricing()
	raw := map[string]ai.StructuredGenerator{
		"fake": fakeai.New(),
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
		raw["anthropic"] = anthropic.New(cfg.AI.Anthropic)
		warnIfUnpriced(pricing, "anthropic", cfg.AI.Anthropic.Model)
	}
	if needed["ollama"] && cfg.AI.Ollama.Model != "" {
		raw["ollama"] = ollama.New(cfg.AI.Ollama)
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
				return nil, fmt.Errorf("ai: route %q names provider %q, which isn't constructible (missing config, e.g. APP_AI_OLLAMA_MODEL, or not implemented until a later task)", promptName, name)
			}
			resolvedRoutes[promptName] = append(resolvedRoutes[promptName], gen)
		}
	}

	defaultGen, ok := providers[cfg.AI.Provider]
	if !ok {
		return nil, fmt.Errorf("ai: APP_AI_PROVIDER %q isn't constructible (missing config, or not implemented until a later task)", cfg.AI.Provider)
	}

	return airouter.New(resolvedRoutes, []ai.StructuredGenerator{defaultGen}), nil
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
