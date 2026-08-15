// Package tools is the typed tool registry (PRD §27-§29, §64): the
// ONLY route an agent has from a ToolCaller conversation to real
// application state (Rule 3 — agents never import ports/storage or the
// application services directly; see .golangci.yml's agents-no-repos
// rule and this package's own tools-not-imported-by-agents rule). Each
// Tool pairs an ai.ToolDef (what the model is told) with a Handler that
// delegates to an existing repository or application service — no new
// business logic lives here, only translation between the model's JSON
// arguments and a real Go call, and back to a compact JSON string the
// model can read.
//
// The single most important property this package enforces: a tool
// Handler's identity/sessionID come ONLY from the arguments Invoke
// itself receives from its caller (the agent-run driver, which knows
// who is really asking) — NEVER from the model-supplied
// ai.ToolInvocation.Arguments JSON. A model that stuffs a forged
// "identity" field into its arguments has no way to reach it: Invoke's
// signature simply has no path from Arguments to the identity a
// Handler runs as.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/domain/session"
	"github.com/mikeyaustin/jlp/internal/ports/ai"
)

// Tool pairs a tool's model-facing definition with the handler that
// actually runs it. Handler's identity/sessionID parameters are always
// supplied by Registry.Invoke's own caller — see the package doc
// comment's security property above.
type Tool struct {
	Def     ai.ToolDef
	Handler func(ctx context.Context, identity learner.IdentityID, sessionID *session.ID, args json.RawMessage) (string, error)
}

// Registry holds every known Tool, keyed by name, plus a per-agent
// allowlist of which tool names that agent may call (PRD §64: each
// agent sees and can invoke only the tools it's explicitly granted,
// never the full catalog by default).
type Registry struct {
	mu      sync.RWMutex
	tools   map[string]Tool
	allowed map[string]map[string]bool
}

// NewRegistry returns an empty Registry: no tools registered, no agent
// allowed to call anything, until Register/Allow are called.
func NewRegistry() *Registry {
	return &Registry{
		tools:   make(map[string]Tool),
		allowed: make(map[string]map[string]bool),
	}
}

// Register adds t to the catalog, keyed by t.Def.Name. Registering a
// second Tool under a name already present replaces the first — the
// caller (this package's wiring, at boot) is trusted not to do that by
// accident; there is no duplicate-name guard because nothing in this
// codebase registers the same name twice outside of a test that means
// to.
func (r *Registry) Register(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Def.Name] = t
}

// Allow grants agent permission to call every name in toolNames, in
// addition to (not replacing) whatever it was already allowed —
// repeated calls accumulate. A name not yet Register()ed can still be
// Allow()ed (order-independent wiring); Invoke will simply report it
// unknown if no matching Tool is ever registered before it's called.
func (r *Registry) Allow(agent string, toolNames ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	set := r.allowed[agent]
	if set == nil {
		set = make(map[string]bool, len(toolNames))
		r.allowed[agent] = set
	}
	for _, name := range toolNames {
		set[name] = true
	}
}

// DefsFor returns the ai.ToolDef for every tool agent is allowed to
// call AND that is actually registered, sorted by name for a
// deterministic wire order (a model shouldn't see its tool list
// reshuffle from one call to the next for no reason). An allowed name
// with no matching registered Tool is silently omitted — Invoke is
// where that mismatch actually surfaces as an error, if the model ever
// tries to call it.
func (r *Registry) DefsFor(agent string) []ai.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	set := r.allowed[agent]
	defs := make([]ai.ToolDef, 0, len(set))
	for name := range set {
		if t, ok := r.tools[name]; ok {
			defs = append(defs, t.Def)
		}
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Invoke runs inv on behalf of agent, scoped to identity/sessionID —
// values Invoke's OWN caller supplies, never read from inv.Arguments
// (see the package doc comment's security property). It never panics
// and never returns a Go error: every failure mode a model's bad input
// can produce — an unknown tool name, a tool this agent isn't allowed
// to call, or the handler itself failing — comes back as an
// ai.ToolResult with IsError true and a message the model can read and
// recover from, so a malformed or malicious tool call can't take down
// the agent-run loop.
func (r *Registry) Invoke(ctx context.Context, agent string, identity learner.IdentityID, sessionID *session.ID, inv ai.ToolInvocation) (result ai.ToolResult) {
	result.ID = inv.ID

	r.mu.RLock()
	allowed := r.allowed[agent][inv.Name]
	t, registered := r.tools[inv.Name]
	r.mu.RUnlock()

	if !registered {
		result.IsError = true
		result.Content = fmt.Sprintf("unknown tool %q", inv.Name)
		return result
	}
	if !allowed {
		result.IsError = true
		result.Content = fmt.Sprintf("tool %q is not permitted for agent %q", inv.Name, agent)
		return result
	}

	// Defensive: a handler bug (e.g. an unchecked type assertion on
	// malformed arguments) must not crash the whole agent-run loop —
	// the model gets a clean error turn instead, same as any other
	// handler failure.
	defer func() {
		if p := recover(); p != nil {
			result.IsError = true
			result.Content = fmt.Sprintf("tool %q panicked: %v", inv.Name, p)
		}
	}()

	content, err := t.Handler(ctx, identity, sessionID, inv.Arguments)
	if err != nil {
		result.IsError = true
		result.Content = err.Error()
		return result
	}
	result.Content = content
	return result
}
