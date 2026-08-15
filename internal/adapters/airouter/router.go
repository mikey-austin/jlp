// Package airouter implements ai.StructuredGenerator (New) and
// ai.ToolCaller (NewToolCaller) as routers over other instances of the
// same capability: which provider chain answers a request depends on
// the request's PromptName (PRD §23/§24's config-driven AI routing),
// and a chain that starts to fail falls through to its next member
// rather than surfacing the first error. The two routers are separate
// types with identical routing/fallback logic — kept apart rather than
// sharing one generic implementation because StructuredRequest and
// ToolRequest are unrelated types with no common request interface,
// and the capabilities themselves are deliberately not merged (see
// ports/ai/toolcall.go's doc comment).
//
// Every chain member passed to New/NewToolCaller is expected to
// already be wrapped by an observability decorator — the router itself
// records nothing; it only decides which already-observed generator(s)
// to try, in what order. That's what makes every attempt (not just the
// one that finally succeeds) show up as its own row in ai_requests: a
// failover from "anthropic" to "ollama" is two recorded attempts, one
// per provider, not one record for whichever happened to answer.
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

// toolRouter is ai.ToolCaller's own router, identical in shape and
// behavior to router above (see New's doc comment) but over
// ai.ToolCaller chains instead of ai.StructuredGenerator ones.
type toolRouter struct {
	routes   map[string][]ai.ToolCaller
	fallback []ai.ToolCaller
}

// NewToolCaller returns an ai.ToolCaller that selects a chain by
// req.PromptName exactly as New does for ai.StructuredGenerator —
// same routing, fallback, and per-attempt-error-logging semantics.
func NewToolCaller(routes map[string][]ai.ToolCaller, fallback []ai.ToolCaller) ai.ToolCaller {
	return &toolRouter{routes: routes, fallback: fallback}
}

func (r *toolRouter) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	chain, routed := r.routes[req.PromptName]
	if !routed {
		chain = r.fallback
	}

	var errs []error
	for _, caller := range chain {
		resp, err := caller.CallWithTools(ctx, req)
		if err == nil {
			return resp, nil
		}
		slog.Warn("airouter: tool-caller provider attempt failed, trying next in chain",
			"prompt_name", req.PromptName, "provider", resp.Provider, "err", err)
		errs = append(errs, fmt.Errorf("%s: %w", resp.Provider, err))
	}

	if len(errs) == 0 {
		return ai.ToolResponse{}, fmt.Errorf("airouter: no tool-caller provider configured for prompt %q", req.PromptName)
	}
	return ai.ToolResponse{}, fmt.Errorf("airouter: all tool-caller providers failed for prompt %q: %w", req.PromptName, errors.Join(errs...))
}
