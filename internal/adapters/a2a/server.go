// Package a2a implements Phase 4 Task 3's A2A (agent-to-agent)
// protocol adapter (PRD §29/§30): it exposes three of JLP's own
// agents — the Writing Reviewer (teacher), the Learner Analyst
// (summary), and the Lesson Planner (lesson) — as A2A "skills" a
// remote agent can discover (GET .well-known/agent-card.json) and
// invoke (POST tasks) over plain HTTP+JSON.
//
// Rule 13 is the point of this whole package (PRD §30): "agents own
// reasoning; application services own state. A remote agent should not
// become a second source of truth for learner state." A remote caller
// gets no capability a local agent-run doesn't already have — every
// task this Server runs goes through the EXACT SAME
// application/agentrun.Runner + internal/tools.Registry allowlists any
// local caller (e.g. the agentic teacher, Phase 4 Task 2) goes
// through. This is enforced by construction, not by convention: Server
// holds a *agentrun.Runner and a *tools.Registry and NOTHING else that
// could reach learner state — no storage.XxxRepository, no
// application-layer service, no direct AI generator. If a skill ever
// seemed to need a tool its underlying agent isn't Allow()ed (in
// cmd/jlp/main.go) to call, the fix is to widen that allowlist at the
// one place it's declared — never to give this package its own side
// door around it. See docs/api/a2a.md for the human-readable version
// of this contract.
//
// Identity: single-learner LAN posture. This package never reads an
// identity from a task payload — WithIdentity/IdentityFrom below are
// the ONLY way a request's identity reaches a handler here, and the
// caller that sets it (internal/adapters/http/server.go, which mounts
// Routes() inside its already-authenticated route group) always reads
// it from the SAME request-scoped identity every other authenticated
// route uses (RequireIdentity + the configured auth.Authenticator),
// never from anything the HTTP client's body controls. See
// TestCreateTaskIgnoresForgedIdentityInBody.
package a2a

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"

	"github.com/go-chi/chi/v5"

	"github.com/mikeyaustin/jlp/internal/application/agentrun"
	"github.com/mikeyaustin/jlp/internal/config"
	"github.com/mikeyaustin/jlp/internal/domain/learner"
	"github.com/mikeyaustin/jlp/internal/tools"
)

// Server is the A2A adapter. Its only two collaborators —
// *agentrun.Runner and *tools.Registry — are exactly what the package
// doc comment's Rule 13 argument depends on: there is no field here a
// future change could wire a repository into without it being an
// obviously out-of-place addition to this struct.
type Server struct {
	runner *agentrun.Runner
	reg    *tools.Registry
	cfg    config.A2A

	// mu/tasks back GET {path}/tasks/{task_id}: since task execution is
	// synchronous (see task.go's handleCreateTask doc comment), this is
	// nothing more than a small response cache of tasks THIS process
	// already computed — never a second source of truth for the run
	// itself, which remains the agent_runs row application/agentrun.Runner
	// already persisted (viewable at /ai/agents). A process restart
	// loses this cache; the underlying agent_runs row does not.
	mu    sync.RWMutex
	tasks map[string]taskRecord
}

// New wires an A2A Server over runner/reg — the same instances
// cmd/jlp/main.go already constructed for the local agent-run path —
// and cfg (APP_A2A_ENABLED/APP_A2A_PATH). New never fails: like every
// other optional feature in this codebase (Anki, MQTT, Summary), main
// only calls New at all when cfg.Enabled is true; internal/adapters/http
// only mounts Routes() when it was given a non-nil Server.
func New(runner *agentrun.Runner, reg *tools.Registry, cfg config.A2A) *Server {
	return &Server{runner: runner, reg: reg, cfg: cfg, tasks: make(map[string]taskRecord)}
}

// Routes returns the adapter's own sub-router, relative to whatever
// prefix the caller mounts it under (cfg.Path, by convention — see
// internal/adapters/http/server.go):
//
//	GET  /.well-known/agent-card.json — the agent card (card.go)
//	POST /tasks                       — run a skill (task.go)
//	GET  /tasks/{task_id}             — read back a task's result
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	r.Get("/.well-known/agent-card.json", s.handleAgentCard)
	r.Post("/tasks", s.handleCreateTask)
	r.Get("/tasks/{task_id}", s.handleGetTask)
	return r
}

// identityCtxKey is an unexported type so no other package can collide
// with this context key by accident (the standard Go context-key
// idiom).
type identityCtxKey struct{}

// WithIdentity returns a context carrying id as the request's
// authenticated identity — called exactly once per request, by
// whatever mounts Routes() (internal/adapters/http/server.go), never
// by a handler in this package. This is the ONLY path an identity
// reaches this package's handlers through; see the package doc
// comment's Identity section.
func WithIdentity(ctx context.Context, id learner.IdentityID) context.Context {
	return context.WithValue(ctx, identityCtxKey{}, id)
}

// IdentityFrom reads back the identity WithIdentity attached, if any.
func IdentityFrom(ctx context.Context) (learner.IdentityID, bool) {
	id, ok := ctx.Value(identityCtxKey{}).(learner.IdentityID)
	return id, ok
}

// writeJSON encodes v as the response body with the given status —
// this package's own copy of internal/adapters/http's identical
// helper: a2a deliberately shares no code with that package (nothing
// to import that would blur "this adapter's only collaborators are
// runner/reg" back into something less obviously true by inspection).
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("a2a: write response", "err", err)
	}
}

// writeError writes {"error": msg} with status.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
