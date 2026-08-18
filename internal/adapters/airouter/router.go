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

// PinnedProvider reports the provider a prompt has been pinned to at
// runtime, or "" when it has not been. Supplied by
// application/settings; nil means "nothing is pinned", which is what
// every caller predating pins gets.
type PinnedProvider func(promptName string) string

// WithPinnedProvider supplies the runtime pin lookup. See
// PinnedProvider.
func WithPinnedProvider(f PinnedProvider) Option {
	return func(o *options) { o.pinned = f }
}

// Option configures a router. Functional rather than more constructor
// parameters so that adding the next one does not touch every caller.
type Option func(*options)

type options struct{ pinned PinnedProvider }

func newOptions(opts []Option) options {
	var o options
	for _, apply := range opts {
		apply(&o)
	}
	return o
}

// pin resolves the pinned provider for promptName, if any, and only if
// it is a provider this router can actually dispatch to. An unknown or
// unconstructed name is IGNORED rather than fatal: a pin is a stored
// preference, and a deployment that later drops that provider from its
// configuration must keep working — it falls through to the configured
// route, which is what the learner had before pinning.
func pin[T any](f PinnedProvider, byName map[string]T, promptName string) (T, bool) {
	var zero T
	if f == nil {
		return zero, false
	}
	name := f(promptName)
	if name == "" {
		return zero, false
	}
	got, ok := byName[name]
	if !ok {
		slog.Warn("airouter: prompt is pinned to a provider this process did not build; using the configured route instead",
			"prompt_name", promptName, "pinned", name)
		return zero, false
	}
	return got, true
}

type router struct {
	routes   map[string][]ai.StructuredGenerator
	fallback []ai.StructuredGenerator
	pinned   PinnedProvider
	// byName is every provider New was given, keyed by its provider
	// name (e.g. "ollama", "agycli") — how GenerateStructured resolves
	// an explicit req.ProviderOverride (Phase 4 Task W item 5) without
	// needing routes/fallback to already name that provider for
	// req.PromptName. nil (every caller before this field existed) is
	// equivalent to "no provider is a valid override target" — a
	// request naming one then always fails with ErrUnknownProvider,
	// never silently falls through to routes/fallback.
	byName map[string]ai.StructuredGenerator
}

// ErrUnknownProvider is returned (wrapped, naming the requested
// provider) when req.ProviderOverride doesn't match any entry in the
// byName map New was given — Phase 4 Task W item 5's "reject an
// unknown/unconfigured provider" requirement. The HTTP handler already
// validates a submitted override against the same constructible-
// provider list before ever constructing the request (a 400, not this
// error, is what a stale form actually surfaces to the browser) — this
// is the router's own last-resort guard for any OTHER caller that
// skips that check.
var ErrUnknownProvider = errors.New("airouter: unknown or unconfigured provider")

// New returns an ai.StructuredGenerator that selects a chain by
// req.PromptName (exact match against routes), falling back to
// fallback when no route names that prompt. Whichever chain is
// selected, its members are tried in order until one succeeds; a
// member that errors is logged and the next is tried. If every member
// of the selected chain fails (or the chain is empty), GenerateStructured
// returns a joined error naming every provider that was tried.
//
// byName is the full set of providers New may dispatch to when a
// request carries an explicit req.ProviderOverride (Phase 4 Task W
// item 5) — normally the SAME map buildAIGenerator already built
// before assembling routes/fallback from it, keyed by provider name.
// It plays no part in ordinary (non-overridden) routing.
func New(routes map[string][]ai.StructuredGenerator, fallback []ai.StructuredGenerator, byName map[string]ai.StructuredGenerator, opts ...Option) ai.StructuredGenerator {
	return &router{routes: routes, fallback: fallback, byName: byName, pinned: newOptions(opts).pinned}
}

func (r *router) GenerateStructured(ctx context.Context, req ai.StructuredRequest) (ai.StructuredResponse, error) {
	// An explicit override is dispatched to EXACTLY that provider, once,
	// with no fallback on failure — the whole point of an operator
	// picking a specific adapter is defeated if a failure there silently
	// answers from a different one instead (see req.ProviderOverride's
	// own doc comment). This check runs before any routes/PromptName
	// lookup, so an override always wins regardless of what (if
	// anything) is configured for req.PromptName.
	if req.ProviderOverride != "" {
		gen, ok := r.byName[req.ProviderOverride]
		if !ok {
			return ai.StructuredResponse{}, fmt.Errorf("%w: %q", ErrUnknownProvider, req.ProviderOverride)
		}
		resp, err := gen.GenerateStructured(ctx, req)
		if err != nil {
			return ai.StructuredResponse{}, fmt.Errorf("%s: %w", req.ProviderOverride, err)
		}
		return resp, nil
	}

	// A runtime pin is the same decision as the override above, made
	// once in settings instead of per request — so it dispatches the
	// same way: that provider, once, no fallback.
	if gen, ok := pin(r.pinned, r.byName, req.PromptName); ok {
		resp, err := gen.GenerateStructured(ctx, req)
		if err != nil {
			return ai.StructuredResponse{}, fmt.Errorf("%s (pinned): %w", resp.Provider, err)
		}
		return resp, nil
	}

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
	byName   map[string]ai.ToolCaller
	pinned   PinnedProvider
}

// NewToolCaller returns an ai.ToolCaller that selects a chain by
// req.PromptName exactly as New does for ai.StructuredGenerator —
// same routing, fallback, and per-attempt-error-logging semantics.
func NewToolCaller(routes map[string][]ai.ToolCaller, fallback []ai.ToolCaller, byName map[string]ai.ToolCaller, opts ...Option) ai.ToolCaller {
	return &toolRouter{routes: routes, fallback: fallback, byName: byName, pinned: newOptions(opts).pinned}
}

func (r *toolRouter) CallWithTools(ctx context.Context, req ai.ToolRequest) (ai.ToolResponse, error) {
	if caller, ok := pin(r.pinned, r.byName, req.PromptName); ok {
		resp, err := caller.CallWithTools(ctx, req)
		if err != nil {
			return ai.ToolResponse{}, fmt.Errorf("%s (pinned): %w", resp.Provider, err)
		}
		return resp, nil
	}

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
