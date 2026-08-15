// Package airouter implements ai.StructuredGenerator as a router over
// other ai.StructuredGenerator instances: which provider chain answers
// a request depends on the request's PromptName (PRD §23/§24's
// config-driven AI routing), and a chain that starts to fail falls
// through to its next member rather than surfacing the first error.
//
// Every chain member passed to New is expected to already be wrapped
// by observability.NewAIObserver — the router itself records nothing;
// it only decides which already-observed generator(s) to try, in what
// order. That's what makes every attempt (not just the one that
// finally succeeds) show up as its own row in ai_requests: a failover
// from "anthropic" to "ollama" is two recorded attempts, one per
// provider, not one record for whichever happened to answer.
package airouter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

type router struct {
	routes   map[string][]ai.StructuredGenerator
	fallback []ai.StructuredGenerator
}

// New returns an ai.StructuredGenerator that selects a chain by
// req.PromptName (exact match against routes), falling back to
// fallback when no route names that prompt. Whichever chain is
// selected, its members are tried in order until one succeeds; a
// member that errors is logged and the next is tried. If every member
// of the selected chain fails (or the chain is empty), GenerateStructured
// returns a joined error naming every provider that was tried.
func New(routes map[string][]ai.StructuredGenerator, fallback []ai.StructuredGenerator) ai.StructuredGenerator {
	return &router{routes: routes, fallback: fallback}
}

func (r *router) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	chain, routed := r.routes[req.PromptName]
	if !routed {
		chain = r.fallback
	}

	var errs []error
	for _, gen := range chain {
		resp, err := gen.GenerateStructured(ctx, req)
		if err == nil {
			return resp, nil
		}
		slog.Warn("airouter: provider attempt failed, trying next in chain",
			"prompt_name", req.PromptName, "provider", resp.Provider, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", resp.Provider, err))
	}

	if len(errs) == 0 {
		return ai.StructuredResponse{}, fmt.Errorf("airouter: no provider configured for prompt %q", req.PromptName)
	}
	return ai.StructuredResponse{}, fmt.Errorf("airouter: all providers failed for prompt %q: %w", req.PromptName, errors.Join(errs...))
}
